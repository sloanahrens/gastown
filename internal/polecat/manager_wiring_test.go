package polecat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Every other Manager test builds its manager with newTestManager, which
// injects a fake bd and a fake tmux. These two tests are the wiring guards
// for NewManager itself: the exported constructor must hand the manager the
// real bd on PATH and the caller's tmux.

// TestNewManagerRunsTheBdOnPath puts a logging bd first on PATH and checks
// that a manager from NewManager reaches it. It changes PATH, so it is not
// parallel.
func TestNewManagerRunsTheBdOnPath(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + logPath + "'\n" +
		"case \" $* \" in *\" show \"*) echo '[]' ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	m := NewManager(&rig.Rig{Name: "rig", Path: root}, nil, nil)
	if err := m.CheckDoltHealth(); err != nil {
		t.Fatalf("CheckDoltHealth: %v", err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "show __health_check_nonexistent__") {
		t.Fatalf("the bd on PATH never saw the health check; its log:\n%s", logged)
	}
}

// TestNewManagerKeepsTheCallersTmux checks that NewManager stores the tmux it
// is given, and stores no tmux at all (not a typed nil, which would defeat
// every "m.tmux != nil" guard) when given none.
func TestNewManagerKeepsTheCallersTmux(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "rig", Path: t.TempDir()}
	if m := NewManager(r, nil, nil); m.tmux != nil {
		t.Fatalf("NewManager(nil tmux).tmux = %#v, want nil", m.tmux)
	}
	tm := tmux.NewTmuxWithSocket("gt-test-unused")
	if m := NewManager(r, nil, tm); m.tmux != tm {
		t.Fatalf("NewManager(tm).tmux = %#v, want the caller's %p", m.tmux, tm)
	}
}
