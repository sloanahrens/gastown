package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Wiring guards: each test below leaves one seam nil and proves the
// production collaborator behind it actually ran. The rest of the package
// replaces these seams, so without a guard a seam whose nil path stopped
// calling the real thing would pass every other test.

// TestHostLoadMeasuresTheRealHost guards hostLoad's nil path: the reading is
// the host's own (its CPU count), not a zero value.
func TestHostLoadMeasuresTheRealHost(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if got := d.hostLoad().NumCPU; got != runtime.NumCPU() {
		t.Errorf("hostLoad().NumCPU = %d, want this host's %d: the nil seam must measure the real host", got, runtime.NumCPU())
	}
}

// TestListOriginBranchesReadsTheRigOrigin guards listOriginBranches' nil
// path: it lists the polecat branches on the rig's real origin remote.
func TestListOriginBranchesReadsTheRigOrigin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	runDeadHolderGit(t, origin, "init", "--bare")

	clone := filepath.Join(townRoot, "gt", "mayor", "rig")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	const branch = "polecat/basalt/gt-issue1+abc123"
	runDeadHolderGit(t, clone, "init")
	runDeadHolderGit(t, clone, "config", "user.email", "test@test.com")
	runDeadHolderGit(t, clone, "config", "user.name", "Test")
	runDeadHolderGit(t, clone, "remote", "add", "origin", origin)
	runDeadHolderGit(t, clone, "checkout", "-b", branch)
	runDeadHolderGit(t, clone, "commit", "--allow-empty", "-m", "work")
	runDeadHolderGit(t, clone, "push", "origin", branch)

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, "gt", 10*time.Minute, nil, nil, nil)
	got, err := m.listOriginBranches(filepath.Join(townRoot, "gt"))
	if err != nil {
		t.Fatalf("listOriginBranches: %v", err)
	}
	if want := []string{branch}; !reflect.DeepEqual(got, want) {
		t.Errorf("listOriginBranches = %v, want %v from the rig's origin", got, want)
	}
}

// TestEnsureDeaconRunning_StartsThroughTheManager guards startDeacon's nil
// path: ensureDeaconRunning really asks deacon.Manager to create the session.
// The bash tmux on PATH refuses the create, so the manager's error must reach
// the log; a startDeacon that did nothing would log a successful start.
// Serial: it sets PATH.
func TestEnsureDeaconRunning_StartsThroughTheManager(t *testing.T) {
	binDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + tmuxLog + "'\n" +
		"case \"$*\" in\n" +
		// A missing session answers the way tmux does; a bare exit 1
		// would be an unknown answer, which starts nothing.
		"  *has-session*) echo \"can't find session: hq-deacon\" >&2; exit 1;;\n" +
		"  *new-session*) echo 'create refused by test' >&2; exit 1;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logs, "", 0),
		tmux:   tmux.NewTmux(),
	}
	d.ensureDeaconRunning()

	data, _ := os.ReadFile(tmuxLog)
	if !strings.Contains(string(data), "new-session") || !strings.Contains(string(data), deacon.SessionName()) {
		t.Errorf("deacon.Manager never tried to create %s; tmux calls:\n%s", deacon.SessionName(), data)
	}
	if !strings.Contains(logs.String(), "Error starting Deacon") {
		t.Errorf("the manager's refused create must be logged as a failed start, got:\n%s", logs.String())
	}
}
