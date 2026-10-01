package cmd

import (
	"os"
	"path/filepath"
	"testing"

)

// TestClosedWispDeleteAge verifies the grace period read for
// purgeClosedEphemeralBeads (gt-1q46): default when unconfigured, the
// configured lifecycle.reaper.delete_age when present, and a fallback to
// the default when the configured value can't be parsed as a duration.
func TestClosedWispDeleteAge(t *testing.T) {
	t.Parallel()
	t.Run("no config file", func(t *testing.T) {
		townRoot := t.TempDir()
		if got := closedWispDeleteAge(townRoot); got != defaultClosedWispDeleteAge {
			t.Errorf("closedWispDeleteAge() = %q, want default %q", got, defaultClosedWispDeleteAge)
		}
	})

	t.Run("configured delete_age", func(t *testing.T) {
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
			t.Fatalf("mkdir mayor: %v", err)
		}
		daemonJSON := `{"type":"daemon-patrol-config","version":1,"patrols":{"wisp_reaper":{"enabled":true,"delete_age":"24h"}}}`
		if err := os.WriteFile(filepath.Join(townRoot, "mayor", "daemon.json"), []byte(daemonJSON), 0644); err != nil {
			t.Fatalf("write daemon.json: %v", err)
		}
		if got := closedWispDeleteAge(townRoot); got != "24h" {
			t.Errorf("closedWispDeleteAge() = %q, want %q", got, "24h")
		}
	})

	t.Run("invalid delete_age falls back to default", func(t *testing.T) {
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
			t.Fatalf("mkdir mayor: %v", err)
		}
		daemonJSON := `{"type":"daemon-patrol-config","version":1,"patrols":{"wisp_reaper":{"enabled":true,"delete_age":"not-a-duration"}}}`
		if err := os.WriteFile(filepath.Join(townRoot, "mayor", "daemon.json"), []byte(daemonJSON), 0644); err != nil {
			t.Fatalf("write daemon.json: %v", err)
		}
		if got := closedWispDeleteAge(townRoot); got != defaultClosedWispDeleteAge {
			t.Errorf("closedWispDeleteAge() = %q, want default %q", got, defaultClosedWispDeleteAge)
		}
	})
}

// TestPurgeClosedEphemeralBeadsPassesOlderThan is the regression test for
// gt-1q46: MR beads are ephemeral wisps (label gt:merge-request) that get
// CLOSED with a valuable reason (a supersede or rejection verdict) moments
// before gt done's self-clean nuke runs purgeClosedEphemeralBeads. Without
// an --older-than grace period, that unconditional `bd purge --force`
// deletes the just-closed MR bead outright, destroying the verdict instead
// of merely closing it. This asserts the grace-period flag actually reaches
// the `bd purge` invocation.
func TestPurgeClosedEphemeralBeadsPassesOlderThan(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	daemonJSON := `{"type":"daemon-patrol-config","version":1,"patrols":{"wisp_reaper":{"enabled":true,"delete_age":"48h"}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "daemon.json"), []byte(daemonJSON), 0644); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}

	var bd recordingPurger
	purgeClosedEphemeralBeads(&bd, townRoot)

	if len(bd.olderThan) != 1 || bd.olderThan[0] != "48h" {
		t.Errorf("purge grace periods = %q, want exactly 48h", bd.olderThan)
	}
}

// recordingPurger records the grace period each purge was asked for.
type recordingPurger struct{ olderThan []string }

func (p *recordingPurger) PurgeClosedEphemeral(olderThan string) (string, error) {
	p.olderThan = append(p.olderThan, olderThan)
	return "0", nil
}
