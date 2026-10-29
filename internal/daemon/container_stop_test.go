package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// gt-qpeei.19: a container town's daemon runs in another namespace, so
// daemon.lock's PID and daemon.pid both name processes on this host only by
// accident. IsRunning never hands a caller one, and StopDaemon refuses before
// it can be signaled.

// supervisorTown makes a town whose daemon.supervisor setting is kind. An
// empty kind writes no file at all, the town that leaves the setting unset.
func supervisorTown(t *testing.T, kind string) string {
	t.Helper()
	townRoot := t.TempDir()
	if kind == "" {
		return townRoot
	}
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "{\"supervisor\": \"" + kind + "\"}"
	if err := os.WriteFile(filepath.Join(mayorDir, "daemon.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// unknownSettingsTown makes a town whose daemon.json is doc, a document that
// leaves the supervisor kind unknown, with a daemon holding the lock as pid.
func unknownSettingsTown(t *testing.T, doc string) string {
	t.Helper()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "daemon.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	holdDaemonLock(t, townRoot, os.Getpid())
	return townRoot
}

// holdDaemonLock leaves the state of a daemon whose flock this process can
// see: daemon.lock held, and daemon.pid naming pid.
func holdDaemonLock(t *testing.T, townRoot string, pid int) {
	t.Helper()
	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(daemonDir, "daemon.lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("hold daemon.lock: locked=%v err=%v", locked, err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
	if _, err := writePIDFile(filepath.Join(daemonDir, "daemon.pid"), pid); err != nil {
		t.Fatal(err)
	}
}

// countedSignals records the signals a stop sends, and sends nothing.
type countedSignals struct {
	terms int
	kills int
}

func (c *countedSignals) signals() daemonStopSignals {
	return daemonStopSignals{
		term: func(*os.Process) error { c.terms++; return nil },
		kill: func(*os.Process) error { c.kills++; return nil },
	}
}

// The container refusal comes before anything that could name a PID: with a
// held lock and a live PID in the pid file, the stop names the container
// supervisor and sends no signal.
func TestStopDaemonContainerTownRefusesWithoutSignaling(t *testing.T) {
	t.Parallel()
	townRoot := supervisorTown(t, agentconfig.SupervisorContainer)
	holdDaemonLock(t, townRoot, os.Getpid())
	sigs := &countedSignals{}

	err := stopDaemonWith(townRoot, sigs.signals())
	if !errors.Is(err, errContainerStopDaemon) {
		t.Fatalf("StopDaemon = %v, want the container town's refusal", err)
	}
	if !strings.Contains(err.Error(), "container supervisor") {
		t.Errorf("refusal = %v, want it to name the container supervisor", err)
	}
	if sigs.terms != 0 || sigs.kills != 0 {
		t.Errorf("sent %d SIGTERM and %d SIGKILL, want none for a container town", sigs.terms, sigs.kills)
	}
}

// The refusal does not depend on a visible lock. Across a Docker Desktop bind
// mount the container's flock is not there to see, and "no daemon is running"
// is not a claim about the container a Mac-side stop may make.
func TestStopDaemonContainerTownRefusesWithoutAVisibleLock(t *testing.T) {
	t.Parallel()
	townRoot := supervisorTown(t, agentconfig.SupervisorContainer)
	sigs := &countedSignals{}

	err := stopDaemonWith(townRoot, sigs.signals())
	if !errors.Is(err, errContainerStopDaemon) {
		t.Fatalf("StopDaemon = %v, want the container town's refusal even with no lock to read", err)
	}
	if sigs.terms != 0 || sigs.kills != 0 {
		t.Errorf("sent %d SIGTERM and %d SIGKILL, want none for a container town", sigs.terms, sigs.kills)
	}
}

// A setting that cannot be read, or names a kind this binary does not know,
// does not make the town a container town, and the stop keeps the host path a
// launchd or systemd town has. The pid gate is what still refuses a signal: the
// lock's PID here is this test process, which is not the town's daemon, so the
// stop fails closed and sends nothing.
func TestStopDaemonUnknownSettingsKeepTheHostPathAndStillGateThePID(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		doc  string
	}{
		{"unparseable", "{"},
		{"unknown kind", `{"supervisor": "s6"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := unknownSettingsTown(t, tt.doc)
			sigs := &countedSignals{}

			err := stopDaemonWith(townRoot, sigs.signals())
			if err == nil {
				t.Fatal("StopDaemon reported a stop of a PID it cannot prove is this town's daemon")
			}
			if !strings.Contains(err.Error(), "is this process") {
				t.Errorf("error = %v, want the PID gate refusing this process", err)
			}
			if sigs.terms != 0 || sigs.kills != 0 {
				t.Errorf("sent %d SIGTERM and %d SIGKILL to an unproven PID", sigs.terms, sigs.kills)
			}
		})
	}
}

// IsRunning answers that the daemon runs from the lock, and never names the
// PID the pid file holds: that number belongs to the container's namespace.
func TestIsRunningContainerTownNeverNamesThePIDFilePID(t *testing.T) {
	t.Parallel()
	townRoot := supervisorTown(t, agentconfig.SupervisorContainer)
	holdDaemonLock(t, townRoot, os.Getpid())

	running, pid, err := IsRunning(townRoot)
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if !running {
		t.Error("IsRunning = false while the lock is held, want the lock's answer")
	}
	if pid != 0 {
		t.Errorf("IsRunning named PID %d from %s, want no pid for a container town", pid, filepath.Join(townRoot, "daemon", "daemon.pid"))
	}
}

// A lock that cannot be probed has a pid-file fallback for a host daemon, and
// a container town must not take it: the file's PID is not a process here.
func TestIsRunningContainerTownProbeErrorNeverFallsBackToThePIDFile(t *testing.T) {
	t.Parallel()
	townRoot := supervisorTown(t, agentconfig.SupervisorContainer)
	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writePIDFile(filepath.Join(daemonDir, "daemon.pid"), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	probe := func(string) (bool, *flock.Flock, error) {
		return false, nil, errors.New("probe refused")
	}

	running, pid, err := isRunningOn(townRoot, true, probe)
	if err == nil {
		t.Fatal("IsRunning reported an answer from a lock it could not probe")
	}
	if running || pid != 0 {
		t.Errorf("IsRunning = (%v, %d), want no answer and no pid for a container town", running, pid)
	}
}

// A setting that cannot be read, or names a kind this binary does not know,
// leaves the town on the host path: IsRunning answers as it did before the
// container rule existed, from the lock and the pid file.
func TestIsRunningUnknownSettingsKeepsTheHostPath(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		doc  string
	}{
		{"unparseable", "{"},
		{"unknown kind", `{"supervisor": "s6"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := unknownSettingsTown(t, tt.doc)

			running, pid, err := IsRunning(townRoot)
			if err != nil {
				t.Fatalf("IsRunning: %v, want the host path's answer", err)
			}
			if !running || pid != os.Getpid() {
				t.Errorf("IsRunning = (%v, %d), want (%v, %d)", running, pid, true, os.Getpid())
			}
		})
	}
}

// Every other kind keeps the pid file's PID: launchd and systemd daemons are
// processes on this host, and a town that sets no kind is one of them.
func TestIsRunningHostKindsStillNameThePIDFilePID(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{agentconfig.SupervisorLaunchd, agentconfig.SupervisorSystemd, ""} {
		name := kind
		if name == "" {
			name = "no kind set"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			townRoot := supervisorTown(t, kind)
			holdDaemonLock(t, townRoot, os.Getpid())

			running, pid, err := IsRunning(townRoot)
			if err != nil {
				t.Fatalf("IsRunning: %v", err)
			}
			if !running || pid != os.Getpid() {
				t.Errorf("IsRunning = (%v, %d), want (%v, %d)", running, pid, true, os.Getpid())
			}
		})
	}
}
