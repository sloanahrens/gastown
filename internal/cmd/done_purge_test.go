package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
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

	rigDir := filepath.Join(townRoot, "gastown")
	var invocations []string
	run := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
		invocations = append(invocations, strings.Join(c.Args, " "))
		return []byte("0\n"), nil, nil
	}
	bd := beads.NewWithBeadsDirAndRunner(rigDir, filepath.Join(rigDir, ".beads"), run)
	purgeClosedEphemeralBeads(bd, townRoot)

	invocation := strings.Join(invocations, "\n")
	if !strings.Contains(invocation, "purge --force --quiet --older-than 48h") {
		t.Errorf("bd purge invocation = %q, want it to contain %q", invocation, "--older-than 48h")
	}
}
