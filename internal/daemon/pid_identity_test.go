package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// gt-p7zy0: every stop path that signals a PID it did not just start must
// prove the PID is the process it means to stop. These tests drive the
// identity decisions on their seams; pid_identity_integration_test.go runs
// the real ps-and-signal chain against processes it spawned.

func TestIsGTDaemonArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/Users/x/.local/bin/gt", "daemon", "run"}, true},
		{[]string{"gt", "daemon", "run", "--verbose"}, true},
		{[]string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}, false},
		{[]string{"gt", "daemon", "stop"}, false},
		{[]string{"sleep", "60"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isGTDaemonArgs(c.args); got != c.want {
			t.Errorf("isGTDaemonArgs(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// fixedProcess is a processInfo that answers every PID with argv and cwd.
func fixedProcess(argv []string, cwd string) processInfo {
	return processInfo{
		args: func(int) []string { return argv },
		cwd:  func(int) string { return cwd },
	}
}

func TestVerifyGTDaemonPIDRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	verify := func(argv []string, pid int) error {
		return verifyGTDaemonPIDOn("darwin", fixedProcess(argv, townRoot), townRoot, pid)
	}

	if err := verify(nil, 4242); err == nil {
		t.Error("an unreadable command line was accepted as the daemon")
	}
	if err := verify([]string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend"}, 4242); err == nil || !strings.Contains(err.Error(), "not the gt daemon") {
		t.Errorf("Docker's backend accepted as the daemon: %v", err)
	}
	daemon := []string{"gt", "daemon", "run"}
	if err := verify(daemon, 4242); err != nil {
		t.Errorf("gt daemon run rejected: %v", err)
	}
	if err := verify(daemon, os.Getpid()); err == nil {
		t.Error("this process accepted as the daemon")
	}
	if err := verify(daemon, 0); err == nil {
		t.Error("PID 0 accepted")
	}
	// Windows has no ps(1): the daemon.lock flock is the guard there.
	if err := verifyGTDaemonPIDOn("windows", fixedProcess(nil, ""), townRoot, 4242); err != nil {
		t.Errorf("windows refused a PID it cannot inspect: %v", err)
	}
}

// gt-l9s6f: `gt daemon run` names no town in its argv; the town is the
// daemon's working directory. A daemon in another town — or one whose cwd
// cannot be read — is not this town's to signal.
func TestVerifyGTDaemonPIDIsTownScoped(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	townRoot := filepath.Join(parent, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling whose name extends the town's: a string-prefix check would
	// call it inside the town.
	prefixSibling := townRoot + "-two"
	link := filepath.Join(parent, "link")
	if err := os.MkdirAll(prefixSibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(townRoot, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		townRoot string
		cwd      string
		wantErr  string // "" = accepted
	}{
		{"town root", townRoot, townRoot, ""},
		{"below the town root", townRoot, filepath.Join(townRoot, "mayor"), ""},
		{"town root spelled through a symlink", link, townRoot, ""},
		{"another town", townRoot, t.TempDir(), "another town"},
		{"prefix sibling", townRoot, prefixSibling, "another town"},
		{"parent of the town", townRoot, parent, "another town"},
		{"cwd unreadable", townRoot, "", "unreadable"},
	}
	for _, c := range cases {
		err := verifyGTDaemonPIDOn("darwin", fixedProcess([]string{"gt", "daemon", "run"}, c.cwd), c.townRoot, 4242)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: rejected: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want one mentioning %q", c.name, err, c.wantErr)
		}
	}
}

func newStopTestManager(t *testing.T, pid int) (*DoltServerManager, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	var mu sync.Mutex
	m := &DoltServerManager{
		config:   &DoltServerConfig{Enabled: true, Port: 13399, Host: "127.0.0.1"},
		townRoot: t.TempDir(),
		logger: func(format string, v ...interface{}) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(&logs, format+"\n", v...)
		},
		runningFn: func() (int, bool) { return pid, true },
	}
	return m, &logs
}

// The Dolt manager's stop path signals the PID isRunning reports, which comes
// from a pid file plus "something answers on the port"; it is signaled only
// once its verifyDolt check proves it is this town's dolt (gt-p7zy0,
// gt-l9s6f). A refusal never signals. A PID provably not this town's dolt
// makes the pid file stale, so it goes; one whose identity could not be read
// keeps it for a later attempt. The PID is this test binary's own, so a
// regression that signaled it would kill the test run, not a host process.
func TestDoltStopRefusalNeverSignals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		err         error
		keepPIDFile bool
	}{
		{"not a dolt sql-server", fmt.Errorf("PID is sleep: %w", doltserver.ErrNotDoltSQLServer), false},
		{"another town's dolt", fmt.Errorf("serves elsewhere: %w", doltserver.ErrOtherTownDolt), false},
		{"identity unverified", fmt.Errorf("ps failed: %w", doltserver.ErrIdentityUnverified), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pid := os.Getpid()
			m, logs := newStopTestManager(t, pid)
			if err := os.MkdirAll(filepath.Join(m.townRoot, "daemon"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := writePIDFile(m.pidFile(), pid); err != nil {
				t.Fatal(err)
			}
			m.verifyDoltFn = func(string, int) error { return c.err }

			if err := m.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if !strings.Contains(logs.String(), "Not stopping PID") {
				t.Errorf("refusal not logged: %q", logs.String())
			}
			_, statErr := os.Stat(m.pidFile())
			if c.keepPIDFile && statErr != nil {
				t.Errorf("pid file removed although identity was only unverified: %v", statErr)
			}
			if !c.keepPIDFile && !os.IsNotExist(statErr) {
				t.Errorf("stale pid file naming a process that is not this town's dolt was kept: %v", statErr)
			}
		})
	}
}

// The chain that SIGTERMed Docker Desktop (gt-p7zy0): an identity-check
// failure in EnsureRunning called doltserver.KillImposters, which signals
// whatever holds GT_DOLT_PORT — under the hermetic harness a Docker
// container port, held on the host by com.docker.backend. Test managers
// route that call through the seam.
func TestEnsureRunningIdentityFailureUsesKillImpostersSeam(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	m.runningFn = func() (int, bool) { return 1234, true }
	m.identityCheckFn = func() error { return errors.New("imposter") }
	calls := 0
	m.killImpostersFn = func() error { calls++; return nil }
	m.sleepFn = func(time.Duration) {}

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if calls != 1 {
		t.Fatalf("killImposters seam calls = %d, want 1", calls)
	}
}

// newTestManager must never fall through to the real KillImposters.
func TestNewTestManagerStubsKillImposters(t *testing.T) {
	t.Parallel()
	if newTestManager(t).killImpostersFn == nil {
		t.Fatal("newTestManager leaves killImpostersFn nil: an identity failure would reach doltserver.KillImposters")
	}
}
