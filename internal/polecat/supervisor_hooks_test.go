package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

var errHookRefused = errors.New("refused: e-stop")

// Start over a session whose agent exited replaces it through the Respawn
// hook (gt-4k3fj.4.1): a refusal (an e-stop) leaves the old session alone
// and is returned.
func TestStartOverDeadSessionGoesThroughRespawnHook(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", "Toast"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := newFakeSessionTmux()
	m := &SessionManager{tmux: tm, rig: &rig.Rig{Name: "test-rig", Path: rigPath}, gits: newWorld().opener()}
	sess := m.SessionName("Toast")
	if err := tm.NewSession(sess, ""); err != nil {
		t.Fatal(err)
	}
	tm.setAlive(sess, false)
	var gotName, gotReason string
	m.SetHooks(SessionHooks{Respawn: func(name, reason string, _ func() error) error {
		gotName, gotReason = name, reason
		return errHookRefused
	}})

	if err := m.Start("Toast", SessionStartOptions{}); !errors.Is(err, errHookRefused) {
		t.Fatalf("Start = %v, want the hook's refusal", err)
	}
	if gotName != "Toast" || gotReason != "polecat start: replace a session whose agent exited" {
		t.Errorf("hook got (%q, %q)", gotName, gotReason)
	}
	if ok, _ := tm.HasSession(sess); !ok {
		t.Error("a refused respawn killed the old session")
	}
}

// A live session is not a respawn: Start reports it running and never asks
// the hook.
func TestStartOverLiveSessionSkipsRespawnHook(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", "Toast"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := newFakeSessionTmux()
	m := &SessionManager{tmux: tm, rig: &rig.Rig{Name: "test-rig", Path: rigPath}, gits: newWorld().opener()}
	sess := m.SessionName("Toast")
	if err := tm.NewSession(sess, ""); err != nil {
		t.Fatal(err)
	}
	tm.setAlive(sess, true)
	m.SetHooks(SessionHooks{Respawn: func(string, string, func() error) error {
		t.Error("Respawn hook called for a live session")
		return nil
	}})
	if err := m.Start("Toast", SessionStartOptions{}); !errors.Is(err, ErrSessionRunning) {
		t.Fatalf("Start = %v, want ErrSessionRunning", err)
	}
}

// A failed startup's kill goes through the Cleanup hook, not tmux; without a
// hook it kills through tmux.
func TestStartupFailureKillUsesCleanupHook(t *testing.T) {
	t.Parallel()
	tm := newFakeSessionTmux()
	m := &SessionManager{tmux: tm, rig: &rig.Rig{Name: "test-rig"}, gits: newWorld().opener()}
	sess := m.SessionName("Toast")
	if err := tm.NewSession(sess, ""); err != nil {
		t.Fatal(err)
	}
	var got []string
	m.SetHooks(SessionHooks{Cleanup: func(name, reason string) error {
		got = append(got, name+": "+reason)
		return nil
	}})
	m.cleanup("Toast", sess, "polecat start: startup blocked")
	if len(got) != 1 || got[0] != "Toast: polecat start: startup blocked" {
		t.Fatalf("cleanup hook calls = %v", got)
	}
	if ok, _ := tm.HasSession(sess); !ok {
		t.Fatal("cleanup killed through tmux as well as the hook")
	}

	m.SetHooks(SessionHooks{})
	m.cleanup("Toast", sess, "polecat start: startup blocked")
	if ok, _ := tm.HasSession(sess); ok {
		t.Fatal("cleanup without a hook left the session")
	}
}

// The manager's housekeeping kills (reuse, repair, orphan reconcile) go
// through the cleanup hook when one is set (gt-4k3fj.4.1).
func TestManagerHousekeepingKillsUseCleanupHook(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	tm := newFakeProbe()
	m := newTestManager(&rig.Rig{Name: "myrig", Path: filepath.Join(townRoot, "myrig")}, nil, tm, newNoDatabaseBd())
	for _, name := range []string{"toast", "nux"} {
		if err := tm.NewSessionWithCommandAndEnv(session.PolecatSessionName(session.DefaultPrefix, name), townRoot, "sleep 300", nil); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	m.SetCleanup(func(name, reason string) error {
		got = append(got, name+": "+reason)
		return nil
	})

	if err := m.killExistingPolecatSession("toast", "reuse"); err != nil {
		t.Fatalf("killExistingPolecatSession = %v", err)
	}
	m.ReconcilePoolWith([]string{"toast"}, []string{"toast", "nux"})

	want := []string{"toast: polecat reuse: clear the existing session", "nux: polecat reconcile: orphan session without a directory"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
	}
	for _, name := range []string{"toast", "nux"} {
		if ok, _ := tm.HasSession(session.PolecatSessionName(session.DefaultPrefix, name)); !ok {
			t.Errorf("%s killed through tmux as well as the hook", name)
		}
	}
}
