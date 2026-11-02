package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// externalContainerTownRoot writes a town root whose mayor/town.json names the
// town's Dolt listener and the Docker container that runs it (gt-xeb1x), and
// returns it.
func externalContainerTownRoot(t *testing.T, container string) string {
	t.Helper()
	townRoot := t.TempDir()
	mayor := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayor, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town","version":2,"name":"t","dolt":{"port":3307,"external_container":"` + container + `"}}`
	if err := os.WriteFile(filepath.Join(mayor, "town.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// doltContainerCommands is the set of commands the container mode changes, each
// run the way its cobra command runs it.
var doltContainerCommands = []struct {
	name string
	run  func() error
}{
	{"start", func() error { return runDoltStart(nil, nil) }},
	{"stop", func() error { return runDoltStop(nil, nil) }},
	{"restart", func() error { return runDoltRestart(nil, nil) }},
	{"status", func() error { return runDoltStatus(nil, nil) }},
	{"kill-imposters", func() error { return runDoltKillImposters(nil, nil) }},
}

// TestDoltContainerCommandsRefuseAnUnusableContainerName: every command that
// resolves the town's Dolt reports a dolt.external_container that is not a
// Docker container name, instead of reading it as a town with no container and
// acting on a host process (gt-xeb1x).
//
//testpolicy:allow parallel — the dolt commands resolve the town root from process-wide cwd and GT_TOWN_ROOT
func TestDoltContainerCommandsRefuseAnUnusableContainerName(t *testing.T) {
	townRoot := externalContainerTownRoot(t, "-v")
	//testpolicy:allow no-chdir — the dolt commands resolve the town root by walking up from the process cwd
	t.Chdir(t.TempDir())
	//testpolicy:allow no-env — GT_TOWN_ROOT is the fallback the commands read when the cwd is outside a town
	t.Setenv("GT_TOWN_ROOT", townRoot)

	for _, tt := range doltContainerCommands {
		err := tt.run()
		if err == nil {
			t.Errorf("gt dolt %s accepted dolt.external_container = %q, want an error", tt.name, "-v")
			continue
		}
		if !strings.Contains(err.Error(), "dolt.external_container") {
			t.Errorf("gt dolt %s error %q does not name dolt.external_container", tt.name, err)
		}
	}
}

// TestDoltContainerCommandsSurfaceDockerFailures: start, stop and restart reach
// Dolt through docker, and a docker that cannot run is the command's error; the
// imposter sweep refuses a container town outright (gt-xeb1x).
//
//testpolicy:allow parallel — the dolt commands resolve the town root from process-wide cwd and GT_TOWN_ROOT
func TestDoltContainerCommandsSurfaceDockerFailures(t *testing.T) {
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	//testpolicy:allow no-chdir — the dolt commands resolve the town root by walking up from the process cwd
	t.Chdir(t.TempDir())
	//testpolicy:allow no-env — GT_TOWN_ROOT is the fallback the commands read when the cwd is outside a town
	t.Setenv("GT_TOWN_ROOT", townRoot)
	//testpolicy:allow no-env — PATH is the lookup that makes docker unfindable, so the test runs no engine
	t.Setenv("PATH", t.TempDir())

	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{"start", func() error { return runDoltStart(nil, nil) }, "docker start"},
		{"stop", func() error { return runDoltStop(nil, nil) }, "docker stop"},
		{"restart", func() error { return runDoltRestart(nil, nil) }, "docker stop"},
		{"kill-imposters", func() error { return runDoltKillImposters(nil, nil) }, "gt never signals a process"},
	}
	for _, tt := range tests {
		err := tt.run()
		if err == nil {
			t.Errorf("gt dolt %s with no docker succeeded, want an error", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("gt dolt %s error %q does not name %q", tt.name, err, tt.want)
		}
	}
}

// TestDoltContainerStatusReportsTheListenerNotThePid: `gt dolt status` in a
// town whose Dolt runs in a container reports the container's listener, not a
// host pid: with nothing answering the SQL ping it prints the container as not
// running even while a live process holds the pid file, names the connection it
// tried, and leaves the pid file alone (gt-zbj2y).
//
//testpolicy:allow parallel — the command resolves the town root from process-wide cwd and GT_TOWN_ROOT, and the report goes to the process stdout
func TestDoltContainerStatusReportsTheListenerNotThePid(t *testing.T) {
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	// The listener is an address no dial can reach: "[" is not a bracketed
	// host, so the SQL ping fails in-process with no resolver, socket, server
	// or Docker, and the report has to say the container is not running.
	townConfig := `{"type":"town","version":2,"name":"t","dolt":{"host":"[","port":9,"external_container":"gt-dolt"}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(townConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	// A pid file naming a live process: the host-process status would call the
	// server running. Container mode reads the listener instead, and must not
	// read or remove the file.
	pidFile := filepath.Join(townRoot, "daemon", "dolt.pid")
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	livePID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(pidFile, []byte(livePID), 0o644); err != nil {
		t.Fatal(err)
	}

	//testpolicy:allow no-chdir — the command resolves the town root by walking up from the process cwd
	t.Chdir(t.TempDir())
	//testpolicy:allow no-env — GT_TOWN_ROOT is the fallback the command reads when the cwd is outside a town
	t.Setenv("GT_TOWN_ROOT", townRoot)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	stdout := os.Stdout
	//testpolicy:allow no-global-swap — the status report goes to the process stdout; the pipe reads it
	os.Stdout = w
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the os.Stdout swapped above
		os.Stdout = stdout
		r.Close()
	})

	statusErr := runDoltStatus(nil, nil)
	w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("reading status output: %v", readErr)
	}
	if statusErr != nil {
		t.Fatalf("gt dolt status over a stopped container: %v", statusErr)
	}

	if want := "Dolt server is not running (container gt-dolt)"; !strings.Contains(string(out), want) {
		t.Errorf("status output does not report the stopped container, want %q:\n%s", want, out)
	}
	if want := "Connection: root@tcp([:9)/"; !strings.Contains(string(out), want) {
		t.Errorf("status output does not name the connection it tried, want %q:\n%s", want, out)
	}
	if strings.Contains(string(out), "PID") {
		t.Errorf("status output reports a host pid in container mode:\n%s", out)
	}

	got, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("status read or removed the pid file: %v", err)
	}
	if string(got) != livePID {
		t.Errorf("pid file = %q, want it left untouched at %q", got, livePID)
	}
}
