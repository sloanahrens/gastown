package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	if len(*hostCalls) != 0 {
		t.Errorf("container mode took host-process path(s) %v, want none", *hostCalls)
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
		stop bool
	}{
		{name: "health tick"},
		{name: "daemon stop", stop: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeExternalContainerTown(t, townRoot, 3307, "-v")
			lifecycle := &fakeContainerLifecycle{running: true}
			m, hostCalls := containerManager(t, townRoot, lifecycle, nil)

			var err error
			if tt.stop {
				err = m.Stop()
			} else {
				err = m.EnsureRunning()
			}
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
