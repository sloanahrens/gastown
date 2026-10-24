package daemon

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/intent"
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

// A liveness sample is written only while the record is the one the assessment
// read. A sling that lands after the read starts a new session with a clean
// record, and the old session's sample must not be written over it.
func TestRecordSeatSampleDoesNotWriteOverASlingSinceTheRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d, seat := liveSeatDaemon(t, townRoot, "ace")
	iseat := supervisor.IntentSeat(seat)
	first := time.Now()
	if err := intent.MarkSlung(townRoot, iseat, "claude", "test", first); err != nil {
		t.Fatal(err)
	}
	read, err := intent.Read(townRoot, iseat)
	if err != nil {
		t.Fatal(err)
	}
	if err := intent.MarkSlung(townRoot, iseat, "claude", "test", first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	d.recordSeatSample(iseat, read, &intent.Progress{DeadSamples: 1})
	if got, err := intent.Read(townRoot, iseat); err != nil || got.Progress != nil {
		t.Fatalf("progress after a sling since the read = %+v (%v), want the cleared record kept", got.Progress, err)
	}
	if !strings.Contains(logs.String(), "not recorded") {
		t.Errorf("log = %q, want the skipped sample reported", logs.String())
	}

	current, err := intent.Read(townRoot, iseat)
	if err != nil {
		t.Fatal(err)
	}
	d.recordSeatSample(iseat, current, &intent.Progress{DeadSamples: 1})
	if got, err := intent.Read(townRoot, iseat); err != nil || got.Progress == nil || got.Progress.DeadSamples != 1 {
		t.Fatalf("progress for a current read = %+v (%v), want the sample written", got.Progress, err)
	}
}

// TestSeatWorkDir pins every branch of seatWorkDir: the directory each seat
// kind's session starts in, and the log line that tells a seat whose worktree
// is missing from one that never had a worktree (a town-level role).
func TestSeatWorkDir(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	tests := []struct {
		name     string
		seat     supervisor.Seat
		mkdirs   []string // relative to the town root
		want     string   // relative to the town root; "" means no directory
		wantLogs bool
	}{
		{
			name:   "nested polecat worktree",
			seat:   supervisor.SeatIn(reg, "gastown", "polecat", "ace"),
			mkdirs: []string{"gastown/polecats/ace/gastown"},
			want:   "gastown/polecats/ace/gastown",
		},
		{
			name:   "nested layout wins over the flat directory",
			seat:   supervisor.SeatIn(reg, "gastown", "polecat", "ace"),
			mkdirs: []string{"gastown/polecats/ace/gastown", "gastown/polecats/ace"},
			want:   "gastown/polecats/ace/gastown",
		},
		{
			name:   "legacy flat polecat worktree",
			seat:   supervisor.SeatIn(reg, "gastown", "polecat", "ace"),
			mkdirs: []string{"gastown/polecats/ace"},
			want:   "gastown/polecats/ace",
		},
		{
			name:     "polecat with no worktree logs what it tried",
			seat:     supervisor.SeatIn(reg, "gastown", "polecat", "ace"),
			wantLogs: true,
		},
		{
			name:   "crew worktree",
			seat:   supervisor.SeatIn(reg, "gastown", "crew", "sloan"),
			mkdirs: []string{"gastown/crew/sloan"},
			want:   "gastown/crew/sloan",
		},
		{
			name:     "crew with no worktree logs what it tried",
			seat:     supervisor.SeatIn(reg, "gastown", "crew", "sloan"),
			wantLogs: true,
		},
		{
			name:   "rig-level role has no worktree and stays quiet",
			seat:   supervisor.SeatIn(reg, "gastown", "witness", ""),
			mkdirs: []string{"gastown/witness"},
		},
		{
			name: "town-level role has no worktree and stays quiet",
			seat: supervisor.SeatIn(reg, "", "mayor", ""),
		},
		{
			name:   "a named seat of a role with no worktree stays quiet",
			seat:   supervisor.SeatIn(reg, "gastown", "refinery", "x"),
			mkdirs: []string{"gastown/refinery/x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			for _, rel := range tt.mkdirs {
				if err := os.MkdirAll(filepath.Join(townRoot, rel), 0o755); err != nil {
					t.Fatalf("creating %s: %v", rel, err)
				}
			}
			var logs bytes.Buffer
			d := &Daemon{
				config: &Config{TownRoot: townRoot},
				logger: log.New(&logs, "", 0),
			}

			got := d.seatWorkDir(tt.seat)

			want := ""
			if tt.want != "" {
				want = filepath.Join(townRoot, tt.want)
			}
			if got != want {
				t.Errorf("seatWorkDir = %q, want %q", got, want)
			}
			if logged := logs.Len() > 0; logged != tt.wantLogs {
				t.Errorf("logged = %v (%q), want %v", logged, logs.String(), tt.wantLogs)
			}
			if tt.wantLogs && !strings.Contains(logs.String(), filepath.Join(townRoot, "gastown")) {
				t.Errorf("log %q does not name the paths it tried", logs.String())
			}
		})
	}
}
