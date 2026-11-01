package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// trimHost is a reap host over a temp town that holds the rig's polecat
// directories. dirCap answers the rig's cap and agents the recorded agent of
// each seat, so no test reads the live registry or a bd.
func trimHost(t *testing.T, dirCap int, agents map[string]string) *patrolScanHost {
	t.Helper()
	h := reapHost(t, enabledCleanup("gastown"), nil, nil)
	writeTownFile(t, h.town(), "settings/config.json", endSettings)
	h.polecatDirCap = func(string) (int, error) { return dirCap, nil }
	h.agentRecord = func(_, name string) (string, error) { return agents[name], nil }
	return h
}

// makePolecatDirs creates one polecat directory per name under the rig.
func makePolecatDirs(t *testing.T, town string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTrimmableSeatsAtDirCap guards gt-xlazq: a rig at its cap names the
// sandbox seats the pass may trim, and names none under the cap. A tmux seat is
// never named: its directory is its seat.
func TestTrimmableSeatsAtDirCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		dirCap int
		want   []string
	}{
		{"at the cap names the sandbox seats", 2, []string{"agate", "cobalt"}},
		{"above the cap names them too", 1, []string{"agate", "cobalt"}},
		{"under the cap names none", 4, nil},
	}
	agents := map[string]string{"agate": endSandboxIn, "basalt": endHostAgent, "cobalt": endSandboxIn}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := trimHost(t, tt.dirCap, agents)
			makePolecatDirs(t, h.town(), "agate", "basalt", "cobalt")

			got, err := h.TrimmableSeats("gastown")
			if err != nil {
				t.Fatalf("TrimmableSeats: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("TrimmableSeats = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPolecatDirCapFor reads the cap from a town's rig registry. The rig bead
// that can raise the cap is read through a client that starts no bd, so this is
// the floor a rig with no max_polecats lands on, and the two reads that cannot
// answer.
func TestPolecatDirCapFor(t *testing.T) {
	t.Parallel()
	const registry = `{"version":1,"rigs":{"gastown":{"git_url":"example.invalid/gastown.git","added_at":"2026-01-01T00:00:00Z"}}}`
	t.Run("a registry that does not list the rig is an error", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownFile(t, town, "mayor/rigs.json", registry)

		if _, err := polecatDirCapFor(town, "nowhere", nil); err == nil {
			t.Fatal("got no error for a rig the registry does not list")
		}
	})

	t.Run("an unreadable registry is an error", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownFile(t, town, "mayor/rigs.json", "not json")

		if _, err := polecatDirCapFor(town, "gastown", nil); err == nil {
			t.Fatal("got no error for a registry that does not decode")
		}
	})

	t.Run("a rig with no max_polecats is floored", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownFile(t, town, "mayor/rigs.json", registry)
		rigPath := filepath.Join(town, "gastown")

		got, err := polecatDirCapFor(town, "gastown", beads.NewUnavailable(rigPath, rigPath))
		if err != nil {
			t.Fatalf("polecatDirCapFor: %v", err)
		}
		if got != rig.MinPolecatDirsPerRig {
			t.Errorf("cap = %d, want the floor %d", got, rig.MinPolecatDirsPerRig)
		}
	})
}

// TestTrimmableSeatsOfAnUncoveredRig: the cleanup config does not cover the
// rig, so the pass does not run there and nothing is trimmed.
func TestTrimmableSeatsOfAnUncoveredRig(t *testing.T) {
	t.Parallel()
	h := trimHost(t, 1, map[string]string{"agate": endSandboxIn})
	makePolecatDirs(t, h.town(), "agate")

	got, err := h.TrimmableSeats("other")
	if err != nil || got != nil {
		t.Fatalf("TrimmableSeats(other) = %v, %v; want nil, nil", got, err)
	}
}

// TestTrimmableSeatsOfARigWithNoDirectories: a rig that has not been slung yet
// holds no seat, so no cap can have been reached.
func TestTrimmableSeatsOfARigWithNoDirectories(t *testing.T) {
	t.Parallel()
	h := trimHost(t, 1, nil)

	got, err := h.TrimmableSeats("gastown")
	if err != nil || got != nil {
		t.Fatalf("TrimmableSeats = %v, %v; want nil, nil", got, err)
	}
}

// TestTrimmableSeatsFailsClosed: a cap or a seat's kind that cannot be read
// trims nothing, because a seat the host cannot classify may be a tmux seat.
func TestTrimmableSeatsFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("cap unreadable", func(t *testing.T) {
		t.Parallel()
		h := trimHost(t, 1, map[string]string{"agate": endSandboxIn})
		makePolecatDirs(t, h.town(), "agate")
		h.polecatDirCap = func(string) (int, error) { return 0, errors.New("registry unreadable") }

		if got, err := h.TrimmableSeats("gastown"); err == nil || got != nil {
			t.Fatalf("TrimmableSeats = %v, %v; want nil and the unreadable cap", got, err)
		}
	})

	t.Run("agent bead unreadable", func(t *testing.T) {
		t.Parallel()
		h := trimHost(t, 1, nil)
		makePolecatDirs(t, h.town(), "agate")
		h.agentRecord = func(_, _ string) (string, error) { return "", errors.New("bd unavailable") }

		if got, err := h.TrimmableSeats("gastown"); err == nil || got != nil {
			t.Fatalf("TrimmableSeats = %v, %v; want nil and the unreadable bead", got, err)
		}
	})

	t.Run("polecats path unreadable", func(t *testing.T) {
		t.Parallel()
		h := trimHost(t, 1, nil)
		rigDir := filepath.Join(h.town(), "gastown")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A file where the directory should be: the listing fails, and the
		// failure is not "does not exist".
		if err := os.WriteFile(filepath.Join(rigDir, "polecats"), nil, 0o644); err != nil {
			t.Fatal(err)
		}

		if got, err := h.TrimmableSeats("gastown"); err == nil || got != nil {
			t.Fatalf("TrimmableSeats = %v, %v; want nil and the unreadable listing", got, err)
		}
	})
}
