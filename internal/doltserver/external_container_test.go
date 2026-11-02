package doltserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// externalContainerTown writes a town root whose mayor/town.json names the
// town's Dolt listener and the Docker container that runs it (gt-xeb1x).
func externalContainerTown(t *testing.T, port int, container string) string {
	t.Helper()
	townRoot := testTown(t)
	mayor := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayor, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town","version":2,"name":"t","dolt":{"port":` + strconv.Itoa(port) + `,"external_container":"` + container + `"}}`
	if err := os.WriteFile(filepath.Join(mayor, "town.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// externalFakeListener answers the host's openDB seam for a container's Dolt:
// it refuses the first failOpens connections and answers every later one, so a
// test can script a container whose Dolt needs time to boot.
type externalFakeListener struct {
	mu        sync.Mutex
	failOpens int
	opens     int
}

func (l *externalFakeListener) open(string) (*sql.DB, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opens++
	if l.opens <= l.failOpens {
		return nil, errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
	}
	return sql.OpenDB(externalFakeConnector{l}), nil
}

func (l *externalFakeListener) openCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}

type externalFakeConnector struct{ listener *externalFakeListener }

func (c externalFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return externalFakeConn{listener: c.listener}, nil
}

func (externalFakeConnector) Driver() driver.Driver { return externalFakeDriver{} }

type externalFakeDriver struct{}

func (externalFakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("the fake connects through externalFakeConnector, not a driver name")
}

type externalFakeConn struct{ listener *externalFakeListener }

func (c externalFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by the container ping")
}

func (externalFakeConn) Close() error { return nil }

func (externalFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the container ping")
}

// Ping is the SQL ping the container mode reads as the server's health.
func (externalFakeConn) Ping(context.Context) error { return nil }

// scriptDockerStart scripts a successful `docker start <name>` on the fake
// machine, the reply a container that exists gives.
func scriptDockerStart(f *fakeHost, name string) {
	f.on("docker start "+name, fakeReply{})
}

// noHostProcess is the assertion every container-mode path shares: nothing was
// spawned and no pid was signalled.
func noHostProcess(t *testing.T, f *fakeHost) {
	t.Helper()
	if starts := f.startedCommands(); len(starts) != 0 {
		t.Errorf("container mode started %d process(es), want none: %v", len(starts), starts)
	}
	f.mu.Lock()
	signals := append([]fakeSignal(nil), f.signals...)
	f.mu.Unlock()
	if len(signals) != 0 {
		t.Errorf("container mode sent %d signal(s), want none: %v", len(signals), signals)
	}
}

// TestExternalContainerStatusUsesTheListenerNotThePid: with
// dolt.external_container set, IsRunning is true when the fake listener answers
// a SQL ping and false when it does not, even while a pid file names a live
// dolt sql-server listening on the port, and no pid file is read or written.
func TestExternalContainerStatusUsesTheListenerNotThePid(t *testing.T) {
	t.Parallel()
	const port = 3307
	townRoot := externalContainerTown(t, port, "gt-dolt")
	f := newFakeHost()
	h := f.host()
	listener := &externalFakeListener{failOpens: 1 << 30}
	h.openDB = listener.open

	// A live dolt sql-server of this town on the port, named by the pid file:
	// the host-process status would call the server running.
	pid := f.doltServer(townRoot, port)
	pidFile := filepath.Join(townRoot, "daemon", "dolt.pid")
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	running, gotPID, err := h.IsRunning(townRoot)
	if err != nil {
		t.Fatalf("IsRunning with the container's listener down: %v", err)
	}
	if running || gotPID != 0 {
		t.Errorf("IsRunning = (%v, %d) with the listener down, want (false, 0): the pid file and port must not answer for a container", running, gotPID)
	}
	if cmds := f.commands(); len(cmds) != 0 {
		t.Errorf("IsRunning ran %q in container mode, want no ps/lsof probe", cmds)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Errorf("IsRunning touched the pid file in container mode: %v", err)
	}

	listener.failOpens = 0
	running, gotPID, err = h.IsRunning(townRoot)
	if err != nil {
		t.Fatalf("IsRunning with the container's listener up: %v", err)
	}
	if !running || gotPID != 0 {
		t.Errorf("IsRunning = (%v, %d) with the listener up, want (true, 0)", running, gotPID)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Errorf("IsRunning touched the pid file in container mode: %v", err)
	}
	noHostProcess(t, f)
}

// TestExternalContainerStartAndStopCallDocker: Start runs docker start and
// waits, bounded, for the SQL ping; a missing container is an error naming it;
// Stop runs docker stop with the host path's stop timeout; Restart does both in
// order; and none of them spawns or signals a host process (gt-xeb1x).
func TestExternalContainerStartAndStopCallDocker(t *testing.T) {
	t.Parallel()
	const port = 3307
	const name = "gt-dolt"

	t.Run("start waits for the ping and runs docker start", func(t *testing.T) {
		t.Parallel()
		townRoot := externalContainerTown(t, port, name)
		f := newFakeHost()
		h := f.host()
		listener := &externalFakeListener{failOpens: 3}
		h.openDB = listener.open
		scriptDockerStart(f, name)

		if err := h.Start(townRoot); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := f.ranMatching("docker start " + name); len(got) != 1 {
			t.Errorf("commands = %q, want exactly one %q", f.commands(), "docker start "+name)
		}
		if opens := listener.openCount(); opens < 4 {
			t.Errorf("Start pinged %d time(s) before answering, want at least 4 (it must wait for the container's Dolt)", opens)
		}
		f.mu.Lock()
		slept := f.slept
		f.mu.Unlock()
		if slept < 2*externalContainerStartPoll {
			t.Errorf("Start slept %v before the listener answered, want it to wait, not accept one refused ping", slept)
		}
		if _, err := os.Stat(filepath.Join(townRoot, "daemon", "dolt.pid")); !os.IsNotExist(err) {
			t.Errorf("Start wrote a pid file for a container: %v", err)
		}
		noHostProcess(t, f)
	})

	t.Run("start names a missing container", func(t *testing.T) {
		t.Parallel()
		townRoot := externalContainerTown(t, port, name)
		f := newFakeHost()
		h := f.host()
		f.on("docker start "+name, fakeReply{stderr: "Error response from daemon: No such container: " + name, code: 1})

		err := h.Start(townRoot)
		if err == nil {
			t.Fatal("Start of a missing container succeeded, want an error")
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name the container %q", err, name)
		}
		if strings.Contains(err.Error(), "No such container") {
			t.Errorf("error %q carries docker's captured output", err)
		}
		noHostProcess(t, f)
	})

	t.Run("start fails when the ping never answers", func(t *testing.T) {
		t.Parallel()
		townRoot := externalContainerTown(t, port, name)
		f := newFakeHost()
		h := f.host()
		h.openDB = (&externalFakeListener{failOpens: 1 << 30}).open
		scriptDockerStart(f, name)

		err := h.Start(townRoot)
		if err == nil {
			t.Fatal("Start with a container that never answers succeeded, want an error")
		}
		for _, want := range []string{name, "127.0.0.1:3307"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
		noHostProcess(t, f)
	})

	t.Run("stop runs docker stop with the stop timeout", func(t *testing.T) {
		t.Parallel()
		townRoot := externalContainerTown(t, port, name)
		f := newFakeHost()
		h := f.host()
		f.on("docker stop -t 5 "+name, fakeReply{})

		if err := h.Stop(townRoot); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if got := f.ranMatching("docker stop"); len(got) != 1 || got[0] != "docker stop -t 5 "+name {
			t.Errorf("commands = %q, want %q", f.commands(), "docker stop -t 5 "+name)
		}
		noHostProcess(t, f)
	})

	t.Run("stop names a container docker cannot stop", func(t *testing.T) {
		t.Parallel()
		townRoot := externalContainerTown(t, port, name)
		f := newFakeHost()
		h := f.host()
		f.on("docker stop -t 5 "+name, fakeReply{stderr: "Error response from daemon: No such container", code: 1})

		err := h.Stop(townRoot)
		if err == nil {
			t.Fatal("Stop of a missing container succeeded, want an error")
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name the container %q", err, name)
		}
		noHostProcess(t, f)
	})

	t.Run("restart stops then starts", func(t *testing.T) {
		t.Parallel()
		townRoot := externalContainerTown(t, port, name)
		f := newFakeHost()
		h := f.host()
		scriptDockerStart(f, name)
		f.on("docker stop -t 5 "+name, fakeReply{})
		h.openDB = (&externalFakeListener{}).open

		if err := h.Restart(townRoot); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		var docker []string
		for _, c := range f.commands() {
			if strings.HasPrefix(c, "docker ") {
				docker = append(docker, c)
			}
		}
		want := []string{"docker stop -t 5 " + name, "docker start " + name}
		if len(docker) != len(want) {
			t.Fatalf("docker commands = %q, want %q", docker, want)
		}
		for i := range want {
			if docker[i] != want[i] {
				t.Errorf("docker command %d = %q, want %q", i, docker[i], want[i])
			}
		}
		noHostProcess(t, f)
	})
}

// TestExternalContainerRefusesAnUnusableSetting: a set dolt.external_container
// that is not a Docker container name is an error from every entry point, never
// a fallback to the host-process server (gt-xeb1x).
func TestExternalContainerRefusesAnUnusableSetting(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTown(t, 3307, "-v")
	f := newFakeHost()
	h := f.host()

	if _, _, err := h.IsRunning(townRoot); err == nil {
		t.Error("IsRunning with dolt.external_container = \"-v\" succeeded, want an error")
	}
	if err := h.Start(townRoot); err == nil {
		t.Error("Start with dolt.external_container = \"-v\" succeeded, want an error")
	}
	if err := h.Stop(townRoot); err == nil {
		t.Error("Stop with dolt.external_container = \"-v\" succeeded, want an error")
	}
	if got := f.ranMatching("docker"); len(got) != 0 {
		t.Errorf("an unusable container name reached docker: %q", got)
	}
	noHostProcess(t, f)
}

// TestExternalContainerKillImpostersRefuses: imposter detection names the port
// holder and signals it, and in container mode that holder is the container's
// own forward. Every caller of KillImposters gets a refusal that names the
// container, and no process is signalled (gt-xeb1x).
func TestExternalContainerKillImpostersRefuses(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTown(t, 3307, "gt-dolt")
	f := newFakeHost()
	h := f.host()
	f.spawn(fakeProc{args: []string{"docker-proxy", "-proto", "tcp"}, port: 3307})

	err := h.KillImposters(townRoot)
	if err == nil {
		t.Fatal("KillImposters in container mode succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "gt-dolt") {
		t.Errorf("error %q does not name the container", err)
	}
	noHostProcess(t, f)
}

// TestDefaultModeIsUnchanged: with dolt.external_container unset, docker is
// never run and the host path starts and stops the server as before.
func TestDefaultModeIsUnchanged(t *testing.T) {
	t.Parallel()
	const port = 3307
	townRoot := testTown(t)
	f := newFakeHost().townPort(port)
	h := f.host()

	pid := f.doltServer(townRoot, port)
	if running, gotPID, err := h.IsRunning(townRoot); err != nil || !running || gotPID != pid {
		t.Fatalf("IsRunning = (%v, %d, %v), want (true, %d, nil) for the host server", running, gotPID, err, pid)
	}
	if err := h.Stop(townRoot); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if f.alive(pid) {
		t.Error("Stop left the host dolt sql-server running")
	}
	if got := f.ranMatching("docker"); len(got) != 0 {
		t.Errorf("the default mode ran docker: %q", got)
	}

	// Restart of a stopped host server is a start, through the same functions.
	f.onStart = func(newPID int, _ *exec.Cmd) { f.listenOn(newPID, port) }
	if err := h.Restart(townRoot); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	starts := f.startedCommands()
	if len(starts) != 1 || len(starts[0].Args) < 2 || starts[0].Args[1] != "sql-server" {
		t.Errorf("host Restart started %v, want one dolt sql-server", starts)
	}
	if got := f.ranMatching("docker"); len(got) != 0 {
		t.Errorf("the default mode's Restart ran docker: %q", got)
	}
}
