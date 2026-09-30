package witness

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/testutil"
)

// G1-15: witness restarts had no budget, so a polecat whose agent died at
// startup was restarted every patrol cycle forever. They now go through the
// supervisor: three per hour, then the seat is frozen and escalated once.
func TestRestartPolecatSession_FourthRestartInAnHourIsRefused(t *testing.T) {
	h := newTestHandlers()
	town := testutil.HermeticTest(t)
	restarts := stubRestartSessionExec(t, h)
	stubHookHold(t, h, "", false)

	for i := 0; i < 4; i++ {
		if err := h.restartPolecatSession(town, "gastown", "flint"); err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
	}

	if len(*restarts) != 3 {
		t.Fatalf("restarts executed = %d, want 3", len(*restarts))
	}
	rec, _ := intent.Read(town, intent.Seat{Rig: "gastown", Role: "polecat", Name: "flint"})
	if !rec.Frozen {
		t.Fatalf("seat not frozen after an exhausted budget: %+v", rec)
	}
	if n := len(recorderOf(t, h).Escalations()); n != 1 {
		t.Fatalf("escalations = %d, want 1", n)
	}
	lines, _ := os.ReadFile(supervisor.ActionLogPath(town))
	if !strings.Contains(string(lines), `"actor":"witness"`) {
		t.Fatalf("witness restarts missing from the action log: %s", lines)
	}
}

// G1-07: a per-rig e-stop stops witness restarts.
func TestRestartPolecatSession_HonorsARigEstop(t *testing.T) {
	h := newTestHandlers()
	town := testutil.HermeticTest(t)
	restarts := stubRestartSessionExec(t, h)
	stubHookHold(t, h, "", false)
	if err := estop.ActivateRig(town, "gastown", estop.TriggerManual, "drill"); err != nil {
		t.Fatal(err)
	}

	if err := h.restartPolecatSession(town, "gastown", "flint"); err != nil {
		t.Fatalf("restart under e-stop returned an error (want a logged refusal): %v", err)
	}
	if len(*restarts) != 0 {
		t.Fatalf("restart executed under a rig e-stop: %v", *restarts)
	}
}

// The nuke's session kill goes through the supervisor; a refusal stops the
// nuke before it touches the worktree.
func TestNukePolecat_PausedPolecatIsNotNuked(t *testing.T) {
	h := newTestHandlers()
	town := testutil.HermeticTest(t)
	var killed, nuked []string
	h.nukeKillSessionFn = func(name string) { killed = append(killed, name) }
	h.nukePolecatWorktreeFn = func(_, address string) error { nuked = append(nuked, address); return nil }
	if err := agentpause.Pause(town, "gastown", "polecat", "flint", "inspecting", "human", ""); err != nil {
		t.Fatal(err)
	}

	err := h.nukeSessionThenWorktree(town, "gastown", "flint")

	if err == nil || len(killed) != 0 || len(nuked) != 0 {
		t.Fatalf("nuke of a paused polecat: err=%v killed=%v nuked=%v; want a refusal and nothing touched", err, killed, nuked)
	}
}
