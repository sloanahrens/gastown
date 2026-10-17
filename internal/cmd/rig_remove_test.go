package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingRigTmux is a tmux server holding sessions, which records the
// sessions removeRigIn asked to stop. Every stop fails, so a registered rig's
// removal ends at the stop and touches nothing else in the town.
type recordingRigTmux struct {
	mu      sync.Mutex
	stopped []string
}

func (r *recordingRigTmux) tmux(sessions ...string) rigRemoveTmux {
	return rigRemoveTmux{
		list: func() ([]string, error) { return sessions, nil },
		stop: func(session, _ string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.stopped = append(r.stopped, session)
			return errors.New("stop refused by test")
		},
	}
}

// rigRemoveTown writes a town whose registry holds the named rigs.
func rigRemoveTown(t *testing.T, rigs ...string) string {
	t.Helper()
	entries := make([]string, len(rigs))
	for i, name := range rigs {
		entries[i] = `"` + name + `":{"git_url":"file:///nonexistent"}`
	}
	town := t.TempDir()
	rigsPath := filepath.Join(town, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(rigsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigsPath, []byte(`{"version":1,"rigs":{`+strings.Join(entries, ",")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// A rig name the registry does not know has no sessions of its own, but its
// prefix falls back to the default "gt", which is the gastown rig's. gt rig
// remove gastwon --force used to stop every gt-* session before it found the
// rig missing (gt-bfo4z).
func TestRigRemoveUnknownRigTouchesNoSessions(t *testing.T) {
	t.Parallel()

	for _, force := range []bool{true, false} {
		name := "no force"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var rec recordingRigTmux
			err := removeRigIn(rigRemoveTown(t), "gastwon", force,
				rec.tmux("gt-witness", "gt-crew-sloan", "gt-furiosa"))
			if err == nil {
				t.Fatal("removeRigIn for an unregistered rig = nil, want the not-found error")
			}
			if !strings.Contains(err.Error(), "gastwon") {
				t.Errorf("error = %v, want it to name the rig", err)
			}
			if strings.Contains(err.Error(), "running sessions") {
				t.Errorf("error = %v: refused over sessions of a rig that is not registered", err)
			}
			if len(rec.stopped) != 0 {
				t.Errorf("stopped %v for an unregistered rig, want no session touched", rec.stopped)
			}
		})
	}
}

// A registered rig keeps the old behavior: without --force its sessions refuse
// the removal and are left running, and with --force they are stopped.
func TestRigRemoveRegisteredRigSessions(t *testing.T) {
	t.Parallel()

	t.Run("no force refuses", func(t *testing.T) {
		t.Parallel()

		var rec recordingRigTmux
		err := removeRigIn(rigRemoveTown(t, "gastown"), "gastown", false, rec.tmux("gt-witness"))
		if err == nil || !strings.Contains(err.Error(), "running sessions") {
			t.Fatalf("removeRigIn = %v, want the running-sessions refusal", err)
		}
		if len(rec.stopped) != 0 {
			t.Errorf("refusal stopped %v, want no session touched", rec.stopped)
		}
	})

	t.Run("force stops its sessions", func(t *testing.T) {
		t.Parallel()

		var rec recordingRigTmux
		err := removeRigIn(rigRemoveTown(t, "gastown"), "gastown", true, rec.tmux("gt-witness", "other-1"))
		if err == nil || !strings.Contains(err.Error(), "failed to kill") {
			t.Fatalf("removeRigIn = %v, want the aborted-kill error from the failing stop", err)
		}
		if got := strings.Join(rec.stopped, ","); got != "gt-witness" {
			t.Errorf("stopped %q, want only the rig's own session gt-witness", got)
		}
	})
}
