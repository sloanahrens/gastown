package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeContainerLifecycle is the container-mode lifecycle doltserver offers,
// recorded instead of run: docker start, docker stop and a SQL ping's answer.
type fakeContainerLifecycle struct {
	mu      sync.Mutex
	running bool
	calls   []string
	// pingErr, when set, is what the SQL ping answers: the container cannot be
	// reached at all.
	pingErr error
}

func (f *fakeContainerLifecycle) Start(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	f.running = true
	return nil
}

func (f *fakeContainerLifecycle) Stop(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	f.running = false
	return nil
}

func (f *fakeContainerLifecycle) IsRunning(string) (bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pingErr != nil {
		return false, 0, f.pingErr
	}
	return f.running, 0, nil
}

func (f *fakeContainerLifecycle) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// writeExternalContainerTown writes townRoot/mayor/town.json naming the town's
// Dolt listener and the container that runs it.
func writeExternalContainerTown(t *testing.T, townRoot string, port int, container string) {
	t.Helper()
	mayor := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town","version":2,"name":"t","dolt":{"port":` + strconv.Itoa(port) + `,"external_container":"` + container + `"}}`
	if err := os.WriteFile(filepath.Join(mayor, "town.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// containerManager is a manager for a town whose Dolt runs in a Docker
// container, with every host-process hook recording its call in hostCalls so a
// test can see that no host path ran.
func containerManager(t *testing.T, townRoot string, lifecycle doltContainerLifecycle, healthErr error) (*DoltServerManager, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var hostCalls []string
	record := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		hostCalls = append(hostCalls, what)
	}
	m := &DoltServerManager{
		config: &DoltServerConfig{
			Enabled:                 true,
			Port:                    3307,
			Host:                    "127.0.0.1",
			DataDir:                 filepath.Join(townRoot, "dolt"),
			LogFile:                 filepath.Join(townRoot, "daemon", "dolt-server.log"),
			RestartDelayStr:         "1s",
			MaxRestartDelayStr:      "1s",
			MaxRestartsInWindow:     5,
			RestartWindowStr:        "10m",
			HealthyResetIntervalStr: "5m",
		},
		townRoot:           townRoot,
		logger:             func(string, ...interface{}) {},
		containerLifecycle: lifecycle,
		healthCheckFn:      func() error { return healthErr },
		sleepFn:            func(time.Duration) {},
		nowFn:              func() time.Time { return time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC) },
		startFn:            func() error { record("host start"); return nil },
		stopFn:             func() { record("host stop") },
		identityCheckFn:    func() error { record("identity check"); return nil },
		killImpostersFn:    func() error { record("kill imposters"); return nil },
		unhealthyAlertFn:   func(error) {},
		crashAlertFn:       func(int) {},
	}
	return m, &hostCalls
}

// TestHealthRecoveryInExternalModeRestartsTheContainer: the daemon's health
// recovery in container mode stops and starts the Docker container through
// internal/doltserver and never the host process path (gt-xeb1x).
func TestHealthRecoveryInExternalModeRestartsTheContainer(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	lifecycle := &fakeContainerLifecycle{running: true}
	m, hostCalls := containerManager(t, townRoot, lifecycle, errors.New("server is not answering"))

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	want := []string{"stop", "start"}
	got := lifecycle.snapshot()
	if len(got) != len(want) {
		t.Fatalf("container calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("container call %d = %q, want %q (docker stop then docker start)", i, got[i], want[i])
		}
	}
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
	}
	if m.process != nil {
		t.Error("container mode tracked a host process")
	}
	if _, err := os.Stat(m.pidFile()); !os.IsNotExist(err) {
		t.Errorf("container mode wrote a pid file: %v", err)
	}
}

// TestEnsureRunning_ExternalContainerStartsAnUnansweredContainer: a tick that
// finds the container not answering starts it through doltserver, never a host
// process (gt-xeb1x).
func TestEnsureRunning_ExternalContainerStartsAnUnansweredContainer(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	lifecycle := &fakeContainerLifecycle{running: false}
	m, hostCalls := containerManager(t, townRoot, lifecycle, nil)

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := lifecycle.snapshot(); len(got) != 1 || got[0] != "start" {
		t.Errorf("container calls = %v, want [start]", got)
	}
	if _, err := os.Stat(m.unhealthySignalFile()); !os.IsNotExist(err) {
		t.Errorf("a stopped container raised an unhealthy signal: %v", err)
	}
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
	}
}

// TestEnsureRunning_ExternalContainerReportsAPingFailure: a container whose
// Dolt does not answer is an incident of its own, not a stopped container to
// start: the tick writes the unhealthy signal, alerts the failure and heals it
// with docker stop and start (gt-0v0bg).
func TestEnsureRunning_ExternalContainerReportsAPingFailure(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	pingErr := errors.New("fake container: the SQL ping timed out")
	lifecycle := &fakeContainerLifecycle{running: true, pingErr: pingErr}
	m, hostCalls := containerManager(t, townRoot, lifecycle, nil)
	var alerts []error
	m.unhealthyAlertFn = func(err error) { alerts = append(alerts, err) }

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := lifecycle.snapshot(); len(got) != 2 || got[0] != "stop" || got[1] != "start" {
		t.Errorf("container calls = %v, want [stop start]: an unresponsive container is restarted", got)
	}
	if len(alerts) != 1 || !errors.Is(alerts[0], pingErr) {
		t.Errorf("unhealthy alerts = %v, want the ping failure %v", alerts, pingErr)
	}
	signal, err := os.ReadFile(m.unhealthySignalFile())
	if err != nil {
		t.Fatalf("no unhealthy signal written: %v", err)
	}
	if !strings.Contains(string(signal), `"reason":"ping_failed"`) {
		t.Errorf("signal file = %s, want reason ping_failed", signal)
	}
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
	}
}

// TestEnsureRunning_ExternalContainerPingFailureDefersToAGCInFlight: a ping
// failure during a scheduled GC is deferred like the health check's other
// restarts, so the container is left to finish the GC (gt-0v0bg).
func TestEnsureRunning_ExternalContainerPingFailureDefersToAGCInFlight(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	lifecycle := &fakeContainerLifecycle{running: true, pingErr: errors.New("fake container: the SQL ping timed out")}
	m, hostCalls := containerManager(t, townRoot, lifecycle, nil)
	m.SetRestartSuppressor(func() bool { return true })

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := lifecycle.snapshot(); len(got) != 0 {
		t.Errorf("container calls = %v, want none: a GC in flight defers the restart", got)
	}
	if _, err := os.Stat(m.unhealthySignalFile()); !os.IsNotExist(err) {
		t.Errorf("a deferred ping failure raised an unhealthy signal: %v", err)
	}
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
	}
}

// TestEnsureRunning_ExternalContainerPingFailureLeavesTheContainerUpWhenAutoRestartIsOff:
// auto_restart:false still reports a ping failure, and leaves the container for
// the operator instead of stopping and starting it (gt-0v0bg).
func TestEnsureRunning_ExternalContainerPingFailureLeavesTheContainerUpWhenAutoRestartIsOff(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	lifecycle := &fakeContainerLifecycle{running: true, pingErr: errors.New("fake container: the SQL ping timed out")}
	m, hostCalls := containerManager(t, townRoot, lifecycle, nil)
	off := false
	m.config.AutoRestart = &off
	var alerts int
	m.unhealthyAlertFn = func(error) { alerts++ }

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := lifecycle.snapshot(); len(got) != 0 {
		t.Errorf("container calls = %v, want none: auto_restart:false forbids the restart", got)
	}
	if alerts != 1 {
		t.Errorf("unhealthy alerts = %d, want 1", alerts)
	}
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
	}
}

// TestIsRunningWithErrorRefusesAnUnusableContainerSetting: an unreadable
// dolt.external_container is carried as the failure, never read as a stopped
// container, by the reader that reports it (gt-0v0bg).
func TestIsRunningWithErrorRefusesAnUnusableContainerSetting(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "-v")
	m, _ := containerManager(t, townRoot, &fakeContainerLifecycle{running: true}, nil)

	m.mu.Lock()
	defer m.mu.Unlock()
	_, running, err := m.isRunningWithError()
	if err == nil {
		t.Error("isRunningWithError read an unusable dolt.external_container as a stopped container, want the setting's error")
	}
	if running {
		t.Error("isRunningWithError reported an unusable dolt.external_container as running")
	}
}

// TestEnsureRunning_ExternalContainerSkipsTheHostIdentityCheck: a healthy
// container is not restarted over the host filesystem's imposter check, which
// cannot see inside the container and whose failure path would signal whatever
// holds the port — the container's own forward (gt-xeb1x).
func TestEnsureRunning_ExternalContainerSkipsTheHostIdentityCheck(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	lifecycle := &fakeContainerLifecycle{running: true}
	m, hostCalls := containerManager(t, townRoot, lifecycle, nil)
	m.identityCheckFn = func() error {
		*hostCalls = append(*hostCalls, "identity check")
		return errors.New("server is an imposter (wrong data directory)")
	}
	m.lastIdentityCheck = time.Time{} // the periodic check is due this tick

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := lifecycle.snapshot(); len(got) != 0 {
		t.Errorf("a healthy container was restarted over the host identity check: %v", got)
	}
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
	}
}

// TestStatus_ExternalContainerReadsTheListener: Status reports the container's
// ping, with no pid, and reports an unusable dolt.external_container as an
// error instead of a stopped server (gt-xeb1x).
func TestStatus_ExternalContainerReadsTheListener(t *testing.T) {
	t.Parallel()
	t.Run("a stopped container is not running, with no pid", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
		m, _ := containerManager(t, townRoot, &fakeContainerLifecycle{running: false}, nil)

		status := m.Status()
		if status.Running {
			t.Error("Status.Running = true for a container that does not answer")
		}
		if status.PID != 0 {
			t.Errorf("Status.PID = %d, want 0: a container has no host pid", status.PID)
		}
		if status.Error != "" {
			t.Errorf("Status.Error = %q, want none", status.Error)
		}
	})

	t.Run("a ping that fails is reported", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
		lifecycle := &fakeContainerLifecycle{running: true, pingErr: errors.New("dial tcp 127.0.0.1:3307: i/o timeout")}
		m, _ := containerManager(t, townRoot, lifecycle, nil)

		status := m.Status()
		if status.Running {
			t.Error("Status.Running = true for a ping that failed")
		}
		if !strings.Contains(status.Error, "i/o timeout") {
			t.Errorf("Status.Error = %q, want the ping failure", status.Error)
		}
	})

	t.Run("an unusable setting is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeExternalContainerTown(t, townRoot, 3307, "-v")
		m, _ := containerManager(t, townRoot, &fakeContainerLifecycle{running: true}, nil)

		status := m.Status()
		if status.Running {
			t.Error("Status.Running = true for an unusable dolt.external_container")
		}
		if status.Error == "" {
			t.Error("Status.Error is empty for an unusable dolt.external_container")
		}
	})
}

// TestExternalContainerRefusedWhenTheSettingIsUnusable: a dolt.external_container
// that is not a Docker container name refuses the daemon's start and stop ticks
// and touches neither the container nor a host process (gt-xeb1x).
func TestExternalContainerRefusedWhenTheSettingIsUnusable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(m *DoltServerManager) error
	}{
		{name: "health tick", run: func(m *DoltServerManager) error { return m.EnsureRunning() }},
		{name: "daemon start", run: func(m *DoltServerManager) error { return m.Start() }},
		{name: "daemon stop", run: func(m *DoltServerManager) error { return m.Stop() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeExternalContainerTown(t, townRoot, 3307, "-v")
			lifecycle := &fakeContainerLifecycle{running: true}
			m, hostCalls := containerManager(t, townRoot, lifecycle, nil)

			err := tt.run(m)
			if err == nil {
				t.Fatal("an unusable dolt.external_container was accepted, want an error")
			}
			if got := lifecycle.snapshot(); len(got) != 0 {
				t.Errorf("an unusable setting still ran %v", got)
			}
			if len(*hostCalls) != 0 {
				t.Errorf("an unusable setting took host-process path(s) %v, want none", *hostCalls)
			}
		})
	}
}
