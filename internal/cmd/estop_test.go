package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/estop"
)

func TestEstopCmdRejectsUnexpectedArgs(t *testing.T) {
	t.Parallel()
	if estopCmd.Args == nil {
		t.Fatal("estopCmd.Args is nil")
	}
	if err := estopCmd.Args(estopCmd, []string{"junk"}); err == nil {
		t.Fatal("estopCmd should reject unexpected positional args")
	}
	if err := estopCmd.Args(estopCmd, nil); err != nil {
		t.Fatalf("estopCmd should accept no positional args: %v", err)
	}
}

func TestRunEstopStatusDoesNotCreateSentinel(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	var buf strings.Builder
	estopStatus(&buf, townRoot)
	out := buf.String()
	if !strings.Contains(out, "No E-stop active.") {
		t.Fatalf("status output = %q, want no-active message", out)
	}
	if _, err := os.Stat(estop.FilePath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("status should not create town-wide ESTOP sentinel, stat err = %v", err)
	}
}

func TestRunEstopStatusReportsPerRigEstop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := estop.ActivateRig(townRoot, "gastown", estop.TriggerManual, "maintenance"); err != nil {
		t.Fatalf("ActivateRig: %v", err)
	}

	var buf strings.Builder
	estopStatus(&buf, townRoot)
	out := buf.String()
	for _, want := range []string{"E-STOP: gastown", "maintenance", "Clear with:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output = %q, want %q", out, want)
		}
	}
	if _, err := os.Stat(estop.FilePath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("status should not create town-wide ESTOP sentinel, stat err = %v", err)
	}
}

// TestAddPausedToStatusBannerNamesAgent (gt-ahik): the PAUSED banner must
// name which agent is parked and why. The original implementation printed a
// bare "PAUSED" line with the reason but no agent, so a town with two paused
// agents produced two indistinguishable lines.
func TestAddPausedToStatusBannerNamesAgent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	var buf strings.Builder
	addPausedToStatus(&buf, townRoot)

	if got := buf.String(); got != "" {
		t.Errorf("no paused agents: banner = %q, want empty", got)
	}

	if err := agentpause.Pause(townRoot, "gastown", "polecat", "flint", "filesystem scan then cache wipe", "mayor", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	buf.Reset()
	addPausedToStatus(&buf, townRoot)
	got := buf.String()

	for _, want := range []string{"gastown/flint", "mayor", "filesystem scan then cache wipe", "PAUSED"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner %q missing %q", got, want)
		}
	}
}
