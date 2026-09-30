package doltserver

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// gt-p7zy0: a port holder or pid-file PID is only a candidate; nothing is
// signaled until its argv shows a dolt sql-server. gt-l9s6f: a dolt serving
// another town's directory is not this town's to signal. These run on a
// fakeHost: the process table stands in for ps and lsof, and every signal the
// adapter sends is recorded instead of delivered. Reading real processes'
// argv, cwd and listening ports is TestIntegrationProcessIdentity's.

var dockerArgs = []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}

// writePIDFile points townRoot's dolt.pid at pid.
func writePIDFile(t *testing.T, h *host, townRoot string, pid int) string {
	t.Helper()
	cfg := h.DefaultConfig(townRoot)
	if err := os.MkdirAll(filepath.Dir(cfg.PidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg.PidFile
}

func TestVerifyDoltSQLServerPID(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	h := f.host()
	docker := f.spawn(fakeProc{args: dockerArgs})
	unreadable := f.spawn(fakeProc{})
	dolt := f.spawn(fakeProc{args: []string{"dolt", "sql-server"}})

	if err := h.VerifyDoltSQLServerPID(docker); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("Docker's port forwarder: %v, want ErrNotDoltSQLServer", err)
	}
	if err := h.VerifyDoltSQLServerPID(unreadable); !errors.Is(err, ErrIdentityUnverified) {
		t.Errorf("unreadable argv: %v, want ErrIdentityUnverified", err)
	}
	if err := h.VerifyDoltSQLServerPID(dolt); err != nil {
		t.Errorf("dolt sql-server rejected: %v", err)
	}
	if err := h.VerifyDoltSQLServerPID(os.Getpid()); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("this process: %v, want ErrNotDoltSQLServer", err)
	}
	if err := h.VerifyDoltSQLServerPID(0); !errors.Is(err, ErrIdentityUnverified) {
		t.Errorf("PID 0: %v, want ErrIdentityUnverified", err)
	}
}

// A non-dolt process holding the Dolt port (com.docker.backend in the gate)
// is skipped with a warning: no signal, and no error for `gt down` to report.
func TestKillImpostersSkipsNonDoltPortHolder(t *testing.T) {
	t.Parallel()
	f := newFakeHost().townPort(4501)
	holder := f.spawn(fakeProc{args: dockerArgs, cwd: testTown(t), port: 4501})

	if err := f.host().KillImposters(testTown(t)); err != nil {
		t.Errorf("KillImposters = %v, want nil (skip with a warning)", err)
	}
	if sigs := f.signalsTo(holder); len(sigs) != 0 {
		t.Fatalf("KillImposters signaled a non-dolt port holder: %v", sigs)
	}
}

// The town's own server is recognized before any identity check, so ps being
// unable to read its argv is not an error either.
func TestKillImpostersOwnServerWithUnreadableArgvIsNotAnError(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4502)
	holder := f.spawn(fakeProc{cwd: townRoot, port: 4502}) // cwd = town root: ours

	if err := f.host().KillImposters(townRoot); err != nil {
		t.Errorf("KillImposters = %v, want nil", err)
	}
	if sigs := f.signalsTo(holder); len(sigs) != 0 {
		t.Fatalf("own server was signaled: %v", sigs)
	}
}

// A verified dolt of another town on this town's port is the imposter the
// check exists for: it gets SIGTERM, and a pid file naming it goes.
func TestKillImpostersTerminatesForeignDolt(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4503)
	h := f.host()
	imposter := f.doltServer(testTown(t), 4503)
	pidFile := writePIDFile(t, h, townRoot, imposter)

	if err := h.KillImposters(townRoot); err != nil {
		t.Fatalf("KillImposters = %v", err)
	}
	if sigs := f.signalsTo(imposter); !slices.Equal(sigs, []syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("signals to the imposter = %v, want one SIGTERM", sigs)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("pid file naming the imposter kept: %v", err)
	}
}

// Stop refuses a pid-file PID that is not dolt, even when IsRunning claims
// it for the town (here: cwd is the town root and it answers on the port).
func TestStopRefusesNonDoltPID(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4504)
	h := f.host()
	holder := f.spawn(fakeProc{args: dockerArgs, cwd: townRoot, port: 4504})
	writePIDFile(t, h, townRoot, holder)
	if running, pid, _ := h.IsRunning(townRoot); !running || pid != holder {
		t.Fatalf("IsRunning = %v/%d; setup did not reach the pid-file branch", running, pid)
	}

	if err := h.Stop(townRoot); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("Stop = %v, want a refusal wrapping ErrNotDoltSQLServer", err)
	}
	if sigs := f.signalsTo(holder); len(sigs) != 0 {
		t.Fatalf("Stop signaled a non-dolt PID: %v", sigs)
	}
}

// Stop refuses when the server is only reachable over TCP with no local
// process it can name (a Docker port forward).
func TestStopRefusesReachableServerWithoutLocalPID(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4505)
	f.reachable["127.0.0.1:4505"] = true
	h := f.host()
	if running, pid, _ := h.IsRunning(townRoot); !running || pid != 0 {
		t.Fatalf("IsRunning = %v/%d; setup did not reach the TCP-only branch", running, pid)
	}

	err := h.Stop(townRoot)
	if err == nil || !strings.Contains(err.Error(), "no verifiable local process") {
		t.Errorf("Stop = %v, want a refusal", err)
	}
	if len(f.signals) != 0 {
		t.Errorf("Stop sent signals: %v", f.signals)
	}
}

// Stop of this town's own verified dolt: SIGTERM, the pid file removed, and
// the state marked stopped.
func TestStopTerminatesOwnServer(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4506)
	h := f.host()
	server := f.doltServer(townRoot, 4506)
	pidFile := writePIDFile(t, h, townRoot, server)

	if err := h.Stop(townRoot); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if sigs := f.signalsTo(server); !slices.Equal(sigs, []syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("signals = %v, want one SIGTERM", sigs)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("pid file kept: %v", err)
	}
	if state, err := LoadState(townRoot); err != nil || state.Running || state.PID != 0 {
		t.Errorf("state after Stop = %+v, %v", state, err)
	}
}

// A server that ignores SIGTERM for the whole wait is SIGKILLed, after
// re-verifying it is still this town's dolt.
func TestStopEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4507)
	h := f.host()
	dataDir := filepath.Join(townRoot, ".dolt-data")
	server := f.spawn(fakeProc{args: []string{"dolt", "sql-server"}, cwd: dataDir, port: 4507, ignoresTERM: true})
	writePIDFile(t, h, townRoot, server)

	if err := h.Stop(townRoot); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if sigs := f.signalsTo(server); !slices.Equal(sigs, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Errorf("signals = %v, want SIGTERM then SIGKILL", sigs)
	}
	if f.slept < 5_000_000_000 {
		t.Errorf("waited %v for the graceful stop, want the 5s window", f.slept)
	}
}

// F1: the orphaned-server fallback after a failed Stop force-kills only a
// verified dolt. The PID here is a live non-dolt port holder that IsRunning
// claims for the town (cwd = town root, answers on the port), so Stop refuses
// without touching the pid file and IsRunning keeps it: the pid file removal
// asserted below can only come from stopOrphanedServer itself.
func TestStopOrphanedServerNeverKillsNonDolt(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4508)
	h := f.host()
	holder := f.spawn(fakeProc{args: dockerArgs, cwd: townRoot, port: 4508})
	pidFile := writePIDFile(t, h, townRoot, holder)
	if err := h.Stop(townRoot); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Fatalf("Stop = %v, want a refusal", err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("precondition: Stop's refusal must leave the pid file: %v", err)
	}

	h.stopOrphanedServer(townRoot, holder)

	if sigs := f.signalsTo(holder); len(sigs) != 0 {
		t.Fatalf("orphan fallback signaled a non-dolt PID: %v", sigs)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("stopOrphanedServer kept a pid file naming a non-dolt process: %v", err)
	}
}

func TestStopOrphanedServerKillsVerifiedDolt(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4509) // nothing listens: Stop finds no server
	victim := f.spawn(fakeProc{args: []string{"dolt", "sql-server"}, cwd: townRoot})

	f.host().stopOrphanedServer(townRoot, victim)

	if sigs := f.signalsTo(victim); !slices.Equal(sigs, []syscall.Signal{syscall.SIGKILL}) {
		t.Errorf("signals to the orphan = %v, want one SIGKILL", sigs)
	}
}

// gt-l9s6f: a dolt that answers to another town's directory passes the "is
// dolt" check but is not this town's to kill; the pid file naming it is stale.
func TestStopOrphanedServerNeverKillsOtherTownsDolt(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost().townPort(4510)
	h := f.host()
	foreign := f.spawn(fakeProc{args: []string{"dolt", "sql-server"}, cwd: testTown(t)})
	pidFile := writePIDFile(t, h, townRoot, foreign)

	h.stopOrphanedServer(townRoot, foreign)

	if sigs := f.signalsTo(foreign); len(sigs) != 0 {
		t.Fatalf("orphan fallback signaled another town's dolt: %v", sigs)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("stopOrphanedServer kept a pid file naming another town's dolt: %v", err)
	}
}

func TestVerifyTownDoltSQLServerPID(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost()
	h := f.host()
	dolt := []string{"dolt", "sql-server"}
	ours := f.spawn(fakeProc{args: dolt, cwd: townRoot})
	theirs := f.spawn(fakeProc{args: dolt, cwd: testTown(t)})
	ownerless := f.spawn(fakeProc{args: dolt})
	notDolt := f.spawn(fakeProc{args: []string{"sleep", "60"}, cwd: townRoot})

	if err := h.VerifyTownDoltSQLServerPID(townRoot, ours); err != nil {
		t.Errorf("this town's dolt rejected: %v", err)
	}
	err := h.VerifyTownDoltSQLServerPID(townRoot, theirs)
	if !errors.Is(err, ErrOtherTownDolt) || !IsStalePIDFileErr(err) {
		t.Errorf("another town's dolt: %v, want ErrOtherTownDolt", err)
	}
	// A dolt with no data-dir, config or cwd to read: nothing shows whose
	// server it is.
	err = h.VerifyTownDoltSQLServerPID(townRoot, ownerless)
	if !errors.Is(err, ErrIdentityUnverified) || IsStalePIDFileErr(err) {
		t.Errorf("ownerless dolt: %v, want ErrIdentityUnverified and no stale-pid-file verdict", err)
	}
	// Not dolt at all reports the non-dolt error, ahead of the town check.
	if err := h.VerifyTownDoltSQLServerPID(townRoot, notDolt); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("non-dolt: %v, want ErrNotDoltSQLServer", err)
	}
}

// Ownership evidence comes in order: --data-dir, then --config, then cwd.
func TestVerifyTownDoltSQLServerPIDReadsDataDirAndConfigFlags(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	other := testTown(t)
	f := newFakeHost()
	h := f.host()
	dataDir := filepath.Join(townRoot, ".dolt-data")
	byDataDir := f.spawn(fakeProc{args: []string{"dolt", "sql-server", "--data-dir", dataDir}, cwd: other})
	byConfig := f.spawn(fakeProc{args: []string{"dolt", "sql-server", "--config=" + filepath.Join(dataDir, "config.yaml")}, cwd: other})
	relative := f.spawn(fakeProc{args: []string{"dolt", "sql-server", "--data-dir", ".dolt-data"}, cwd: townRoot})
	wrongDataDir := f.spawn(fakeProc{args: []string{"dolt", "sql-server", "--data-dir", filepath.Join(other, ".dolt-data")}, cwd: townRoot})

	for name, pid := range map[string]int{"--data-dir": byDataDir, "--config": byConfig, "relative --data-dir": relative} {
		if err := h.VerifyTownDoltSQLServerPID(townRoot, pid); err != nil {
			t.Errorf("%s: %v, want this town's", name, err)
		}
	}
	if err := h.VerifyTownDoltSQLServerPID(townRoot, wrongDataDir); !errors.Is(err, ErrOtherTownDolt) {
		t.Errorf("--data-dir of another town beats a matching cwd: %v, want ErrOtherTownDolt", err)
	}
}

// killTownDolt is the force-kill for this town's own server: the same
// verdicts, and a foreign town's dolt is never signaled.
func TestKillTownDoltSparesOtherTownsDolt(t *testing.T) {
	t.Parallel()
	townRoot := testTown(t)
	f := newFakeHost()
	foreign := f.spawn(fakeProc{args: []string{"dolt", "sql-server"}, cwd: testTown(t)})

	if err := f.host().killTownDolt(townRoot, foreign); !errors.Is(err, ErrOtherTownDolt) {
		t.Errorf("killTownDolt = %v, want ErrOtherTownDolt", err)
	}
	if sigs := f.signalsTo(foreign); len(sigs) != 0 {
		t.Fatalf("killTownDolt signaled another town's dolt: %v", sigs)
	}
}

// F9: Start's squatter eviction leaves a non-dolt port holder alone.
func TestEvictPortSquatterLeavesNonDoltHolder(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	holder := f.spawn(fakeProc{args: dockerArgs, port: 4511})

	if got := f.host().evictPortSquatter(4511); got != 0 {
		t.Errorf("evictPortSquatter acted on PID %d, want 0", got)
	}
	if sigs := f.signalsTo(holder); len(sigs) != 0 {
		t.Fatalf("squatter eviction signaled a non-dolt holder: %v", sigs)
	}
}

func TestEvictPortSquatterKillsVerifiedDolt(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	h := f.host()
	squatter := f.spawn(fakeProc{args: []string{"dolt", "sql-server"}, port: 4512})

	if got := h.evictPortSquatter(4512); got != squatter {
		t.Errorf("evictPortSquatter = %d, want %d", got, squatter)
	}
	if sigs := f.signalsTo(squatter); !slices.Equal(sigs, []syscall.Signal{syscall.SIGKILL}) {
		t.Errorf("signals = %v, want one SIGKILL", sigs)
	}
	if err := h.portFree(4512); err != nil {
		t.Errorf("port still held after eviction: %v", err)
	}
}
