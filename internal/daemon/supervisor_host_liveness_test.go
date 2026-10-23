package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// writeSeatTranscript files a transcript for worktree the way a seat spawned
// with the town's .claude-town config dir does, and returns its path.
func writeSeatTranscript(t *testing.T, townRoot, worktree string) string {
	t.Helper()
	projectDir, err := agentlog.ClaudeProjectDirIn(filepath.Join(townRoot, ".claude-town"), worktree)
	if err != nil {
		t.Fatalf("ClaudeProjectDirIn: %v", err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}
	path := filepath.Join(projectDir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("writing transcript: %v", err)
	}
	return path
}

// liveSeatDaemon returns a daemon over townRoot with a live session for a polecat
// named name, so assessSeat reaches the sample step.
func liveSeatDaemon(t *testing.T, townRoot, name string) (*Daemon, supervisor.Seat) {
	t.Helper()
	clk := newFixedClock()
	ft := newFakeTmux(clk)
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		tmux:   ft,
		logger: log.New(io.Discard, "", 0),
		clock:  clk,
	}
	seat := supervisor.SeatIn(session.NewPrefixRegistry(), "gastown", "polecat", name)
	ft.addSession(seat.SessionName(), "claude", clk.Now())
	return d, seat
}

// TestAssessSeat_SetsTranscriptFromTownConfig pins gt-3io4b: the daemon's
// liveness input named no work directory, so sample() skipped the transcript
// lookup entirely and no seat's transcript was ever read.
func TestAssessSeat_SetsTranscriptFromTownConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// The nested layout is the cwd polecat sessions start in, and Claude names
	// the transcript's project directory after that path.
	worktree := filepath.Join(townRoot, "gastown", "polecats", "ace", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("creating worktree: %v", err)
	}
	want := writeSeatTranscript(t, townRoot, worktree)
	d, seat := liveSeatDaemon(t, townRoot, "ace")

	res := d.assessSeat(seat, liveness.Input{})

	if res.Verdict != liveness.Alive {
		t.Fatalf("verdict = %v (%s), want Alive", res.Verdict, res.Reason)
	}
	if res.Sample == nil || res.Sample.TranscriptPath != want {
		t.Fatalf("transcript = %+v, want %q", res.Sample, want)
	}
}

// TestAssessSeat_NoTranscriptIsNotDead keeps the dead rule off the transcript:
// a seat with no transcript is sampled without one, not killed for it.
func TestAssessSeat_NoTranscriptIsNotDead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	worktree := filepath.Join(townRoot, "gastown", "polecats", "ace", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("creating worktree: %v", err)
	}
	d, seat := liveSeatDaemon(t, townRoot, "ace")

	res := d.assessSeat(seat, liveness.Input{})

	if res.Verdict != liveness.Alive {
		t.Fatalf("verdict = %v (%s), want Alive", res.Verdict, res.Reason)
	}
	if res.Sample == nil || res.Sample.TranscriptPath != "" {
		t.Fatalf("transcript = %+v, want none", res.Sample)
	}
}
