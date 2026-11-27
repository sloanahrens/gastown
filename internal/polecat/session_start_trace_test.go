package polecat

import (
	"slices"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// TestSessionStartTraceMarksTheWaits: the sling's "session start" phase used
// to cover every startup wait, so a 161 s start could not be attributed to the
// wait that held it (gt-p97r7p). A launch marks the agent-command wait, the
// startup dialogs and the runtime-ready wait as their own phases, in the order
// they run, so the sling's later "session start" mark covers only what follows.
func TestSessionStartTraceMarksTheWaits(t *testing.T) {
	t.Parallel()
	town, rigPath, workDir, w := launchTown(t)
	// The agent has no ready prompt. The launch's background startup-nudge
	// check therefore returns before it reads anything (verifyStartupNudge),
	// so it leaves nothing for this test to join, and the runtime-ready wait
	// returns at once instead of polling the fake's screen.
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	ts.Agents = map[string]*config.RuntimeConfig{
		"trace-seat": {Command: "claude", Args: []string{"--dangerously-skip-permissions"}, Tmux: &config.RuntimeTmuxConfig{}},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
		t.Fatal(err)
	}

	m := &SessionManager{
		tmux: newFakeSessionTmux(),
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
	}
	trace := session.NewSandboxTrace(clockwork.NewFakeClockAt(testEpoch).Now)

	err = m.Start("box", SessionStartOptions{
		WorkDir:          workDir,
		RuntimeConfigDir: t.TempDir(),
		Agent:            "trace-seat",
		Trace:            trace,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got []string
	for _, p := range trace.Breakdown().Phases {
		got = append(got, p.Name)
	}
	want := []string{"agent command wait", "startup dialogs", "runtime ready wait"}
	if !slices.Equal(got, want) {
		t.Errorf("trace phases = %v, want exactly %v in that order", got, want)
	}
}
