package cmd

import (
	"os"
	"path/filepath"
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
