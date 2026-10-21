package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/progress"
)

func TestJevProgressPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_progress") {
		t.Error("jev_progress must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_progress") {
		t.Error("jev_progress must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevProgress: &JevProgressConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_progress") {
		t.Error("an explicit enabled=false must keep jev_progress off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevProgress: &JevProgressConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_progress") {
		t.Error("an explicit enabled=true must turn jev_progress on")
	}
	// Turning the dispatch patrol on leaves the progress patrol off.
	dispatchOnly := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: true}}}
	if IsPatrolEnabled(dispatchOnly, "jev_progress") {
		t.Error("enabling jev_dispatch must not enable jev_progress")
	}
}

// With the patrol off, a guard nudge queues nothing: a town that has not opted
// in does no work and holds no memory for the battery.
func TestJevProgressNudgeQueuesNothingWhenOff(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-hurgx"})
	s := f.seams()
	s.nudged = d.enqueueJevProgress

	d.runOverrunGuard(s)

	if len(f.nudges) != 1 {
		t.Fatalf("nudges = %v, want the guard to nudge once", f.nudges)
	}
	if n := d.jevProgressQueue.Len(); n != 0 {
		t.Errorf("queue holds %d nudges with the patrol off", n)
	}
}

// A nudge the guard delivers queues one ask for that seat. A nudge that failed
// to deliver queues nothing, and neither does a dry run.
func TestJevProgressGuardNudgeEnqueuesOneAsk(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	d.patrolConfig.Patrols.JevProgress = &JevProgressConfig{Enabled: true}
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-hurgx"}

	f := newOverrunFake(13*time.Minute, seat)
	s := f.seams()
	s.nudged = d.enqueueJevProgress
	d.runOverrunGuard(s)

	if len(f.nudges) != 1 {
		t.Fatalf("nudges = %v, want one", f.nudges)
	}
	if n := d.jevProgressQueue.Len(); n != 1 {
		t.Fatalf("queue holds %d nudges, want 1", n)
	}
	got := d.jevProgressQueue.Drain()[0]
	if got.Rig != "gastown" || got.Polecat != "capable" || got.Bead != "gt-hurgx" {
		t.Errorf("queued nudge names the wrong seat: %+v", got)
	}
	if got.At.IsZero() || got.Session == "" {
		t.Errorf("queued nudge has no time or session: %+v", got)
	}

	// The same seat, nudged again on a later tick: the guard does not
	// re-nudge a seat it already nudged, so the queue stays at the one ask.
	d.runOverrunGuard(s)
	if n := d.jevProgressQueue.Len(); n != 0 {
		t.Errorf("a second tick queued %d more asks, want none", n)
	}

	// A fresh town: the guard's state for this bead already records the nudge
	// above, so re-using d would skip the nudge and prove nothing.
	failD, _ := newOverrunDaemon(t, false)
	failD.patrolConfig.Patrols.JevProgress = &JevProgressConfig{Enabled: true}
	failing := newOverrunFake(13*time.Minute, seat)
	failing.nudgeErr = fmt.Errorf("session gone")
	fs := failing.seams()
	fs.nudged = failD.enqueueJevProgress
	failD.runOverrunGuard(fs)
	if len(failing.nudges) != 0 {
		t.Fatalf("the failing nudge was delivered: %v", failing.nudges)
	}
	if n := failD.jevProgressQueue.Len(); n != 0 {
		t.Errorf("a nudge that did not deliver queued %d asks", n)
	}

	dry, _ := newOverrunDaemon(t, true)
	dry.patrolConfig.Patrols.JevProgress = &JevProgressConfig{Enabled: true}
	df := newOverrunFake(13*time.Minute, seat)
	ds := df.seams()
	ds.nudged = dry.enqueueJevProgress
	dry.runOverrunGuard(ds)
	if n := dry.jevProgressQueue.Len(); n != 0 {
		t.Errorf("a dry run queued %d asks", n)
	}
}

// progressJev answers every request with progressAnswerFixture and keeps the
// request body it was sent. It stands in for the Jev endpoint, so no test opens
// a socket.
type progressJev struct {
	req []byte
}

func (p *progressJev) RoundTrip(r *http.Request) (*http.Response, error) {
	p.req, _ = io.ReadAll(r.Body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(progressAnswerFixture)),
		Request:    r,
	}, nil
}

// The pass reads a nudge's transcript through the production Jev client and
// logs one line with the five answers; the line is the only thing it writes.
func TestJevProgressPassLogsOneLineFromFixtureTranscript(t *testing.T) {
	t.Parallel()
	rt := &progressJev{}

	town := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(io.Discard, "", 0),
	}
	transcript := writeProgressTranscript(t, town)
	d.jevProgressQueue.Push(progress.Nudge{
		Rig: "gastown", Polecat: "capable", Bead: "gt-hurgx",
		Session: "gt-capable", At: progressNow.Add(-time.Minute), Transcript: transcript,
	})
	d.jevProgressOpen = func(townRoot string, q *progress.Queue, _ progress.Settings, _ io.Writer) (*progress.Pass, error) {
		client, err := jev.NewClient(
			jev.WithHTTPClient(&http.Client{Transport: rt}),
			jev.WithBaseURL("https://jev.test/v1/systemone"),
			jev.WithKey("test-key"),
		)
		if err != nil {
			return nil, err
		}
		return &progress.Pass{
			Asker:    client,
			Queue:    q,
			TownRoot: townRoot,
			LogPath:  progress.LogPath(townRoot),
			Allows:   func() (bool, error) { return true, nil },
			Now:      func() time.Time { return progressNow },
			Issue: func(rig, id string) (*beads.Issue, error) {
				return &beads.Issue{ID: id, Title: "Polecat progress battery"}, nil
			},
		}, nil
	}

	d.runJevProgress()

	if !bytes.Contains(rt.req, []byte("making_progress")) {
		t.Errorf("the request carries no question set: %s", rt.req)
	}
	var lines []progress.Line
	if err := battery.ReadRecent(progress.LogPath(town), 10, &lines); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	if n := len(lines[0].Answers); n != 5 {
		t.Errorf("answers = %d, want 5", n)
	}
	if lines[0].Bead != "gt-hurgx" || lines[0].Reason != "" {
		t.Errorf("line = %+v", lines[0])
	}
	if d.jevProgressQueue.Len() != 0 {
		t.Errorf("queue holds %d nudges after the pass", d.jevProgressQueue.Len())
	}
}

// The trigger takes no pass when nothing is queued, and takes the single-flight
// guard only when there is work.
func TestJevProgressTriggerWaitsForAQueuedNudge(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	d.patrolConfig.Patrols.JevProgress = &JevProgressConfig{Enabled: true}

	d.triggerJevProgress()
	if d.jevProgressRunning.Load() {
		t.Error("the trigger took the single-flight guard with nothing queued")
	}
}

// The guard's nudge finds the seat's worktree and asks for its live transcript,
// and queues the nudge with that path. A seat with no worktree asks for nothing
// and queues its nudge with no transcript, so its line logs no_transcript. The
// lookup is a seam, so the test does not reach the Claude config dir.
func TestJevProgressEnqueueFindsTheSeatTranscript(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	worktree := filepath.Join(town, "gastown", "polecats", "capable", "gastown")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(town, "claude", "session.jsonl")

	d, _ := newOverrunDaemon(t, false)
	d.config.TownRoot = town
	d.patrolConfig.Patrols.JevProgress = &JevProgressConfig{Enabled: true}
	var looked []string
	d.jevProgressLatest = func(workDir string) (string, error) {
		looked = append(looked, workDir)
		return transcript, nil
	}
	now := time.Now()
	d.enqueueJevProgress(overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-hurgx"}, now)
	d.enqueueJevProgress(overrunSeat{Rig: "gastown", Polecat: "ghost", Bead: "gt-hurgx"}, now)

	if len(looked) != 1 || looked[0] != worktree {
		t.Fatalf("transcript lookups = %v, want one lookup in the seat's worktree %s", looked, worktree)
	}
	got := d.jevProgressQueue.Drain()
	if len(got) != 2 {
		t.Fatalf("queue holds %d nudges, want 2", len(got))
	}
	if got[0].Transcript != transcript {
		t.Errorf("seat transcript = %q, want %q", got[0].Transcript, transcript)
	}
	if got[0].Worktree != worktree {
		t.Errorf("seat worktree = %q, want %q: the pass allows that worktree's projects directories", got[0].Worktree, worktree)
	}
	if got[1].Transcript != "" || got[1].Worktree != "" {
		t.Errorf("a seat with no worktree got transcript %q worktree %q, want neither", got[1].Transcript, got[1].Worktree)
	}
}

// A pass that stops partway logs its counts under its own line, so the log
// never reads like a clean pass that dropped nothing.
func TestJevProgressStoppedPassLogsItsCounts(t *testing.T) {
	t.Parallel()
	rt := &progressJev{}
	town := t.TempDir()
	var buf bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&buf, "", 0),
	}
	transcript := writeProgressTranscript(t, town)
	d.jevProgressQueue.Push(progress.Nudge{
		Rig: "gastown", Polecat: "capable", Bead: "gt-hurgx",
		Session: "gt-capable", At: progressNow.Add(-time.Minute), Transcript: transcript,
	})
	// A regular file where the log's directory should be makes the append fail.
	blocker := filepath.Join(town, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.jevProgressOpen = func(townRoot string, q *progress.Queue, _ progress.Settings, _ io.Writer) (*progress.Pass, error) {
		client, err := jev.NewClient(
			jev.WithHTTPClient(&http.Client{Transport: rt}),
			jev.WithBaseURL("https://jev.test/v1/systemone"),
			jev.WithKey("test-key"),
		)
		if err != nil {
			return nil, err
		}
		return &progress.Pass{
			Asker:    client,
			Queue:    q,
			TownRoot: townRoot,
			LogPath:  filepath.Join(blocker, "progress.jsonl"),
			Allows:   func() (bool, error) { return true, nil },
			Now:      func() time.Time { return progressNow },
			Issue: func(rig, id string) (*beads.Issue, error) {
				return &beads.Issue{ID: id, Title: "Polecat progress battery"}, nil
			},
		}, nil
	}

	d.runJevProgress()

	got := buf.String()
	if !strings.Contains(got, "jev_progress: pass stopped:") || !strings.Contains(got, "(asked 0, unanswered 0, dropped 1)") {
		t.Errorf("log = %q, want a pass-stopped line with its counts", got)
	}
	if strings.Contains(got, "daily cap reached") {
		t.Errorf("a stopped pass logged the clean-pass line: %q", got)
	}
}

// progressNow is the clock the pass test runs on.
var progressNow = time.Date(2026, 10, 21, 0, 30, 0, 0, time.UTC)

// progressAnswerFixture is what the endpoint answers: the five questions, with
// probabilities that sum to one as the client checks.
const progressAnswerFixture = `{
  "model": "jev-1.13.0",
  "answers": {
    "making_progress":        {"type": "noul", "noul": 0.9},
    "looping":                {"type": "noul", "noul": 0.1},
    "blocked_on_environment": {"type": "noul", "noul": 0.2},
    "drifted_from_bead":      {"type": "noul", "noul": 0.05},
    "remaining": {
      "type": "score", "score": 2, "confidence": 0.8,
      "legend": {"0": "done", "1": "nearly done", "2": "halfway", "3": "barely started"},
      "probabilities": {"0": 0.05, "1": 0.15, "2": 0.7, "3": 0.1}
    }
  },
  "usage": {"input_tokens": 400, "output_tokens": 50}
}`

// writeProgressTranscript writes a small Claude Code transcript under the
// town, inside the window before progressNow, and returns its path.
func writeProgressTranscript(t *testing.T, town string) string {
	t.Helper()
	dir := filepath.Join(town, ".claude-town", "projects", "capable")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		progressLine("assistant", "2026-10-21T00:20:00Z", map[string]any{"type": "text", "text": "Reading the guard."}),
		progressLine("assistant", "2026-10-21T00:22:00Z", map[string]any{
			"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]string{"command": "go test ./internal/jev/progress/..."},
		}),
		progressLine("user", "2026-10-21T00:23:00Z", map[string]any{
			"type": "tool_result", "tool_use_id": "t1", "content": "ok",
		}),
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func progressLine(typ, ts string, block map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"type": typ, "timestamp": ts,
		"message": map[string]any{"role": typ, "content": []any{block}},
	})
	return string(b)
}

// TestJevProgressPassStopsBetweenCallsWhenEstopped covers gt-dlnkz: the E-stop
// kept a pass from starting, but one already in flight went on paying for every
// queued nudge. The call in flight finishes and is logged; the nudges behind it
// are dropped, as a stopped pass's are.
func TestJevProgressPassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var buf bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&buf, "", 0),
	}
	transcript := writeProgressTranscript(t, town)
	for _, bead := range []string{"gt-a", "gt-b", "gt-c"} {
		d.jevProgressQueue.Push(progress.Nudge{
			Rig: "gastown", Polecat: "capable", Bead: bead,
			Session: "gt-capable", At: progressNow.Add(-time.Minute), Transcript: transcript,
		})
	}
	asker := &estoppingAsker{t: t, town: town}
	d.jevProgressOpen = func(townRoot string, q *progress.Queue, _ progress.Settings, _ io.Writer) (*progress.Pass, error) {
		return &progress.Pass{
			Asker:    asker,
			Queue:    q,
			TownRoot: townRoot,
			LogPath:  progress.LogPath(townRoot),
			Allows:   func() (bool, error) { return true, nil },
			Now:      func() time.Time { return progressNow },
			Issue: func(rig, id string) (*beads.Issue, error) {
				return &beads.Issue{ID: id, Title: "Polecat progress battery"}, nil
			},
		}, nil
	}

	d.runJevProgress()

	if asker.calls != 1 {
		t.Errorf("Jev was asked about %d nudges, want only the one in flight when the E-stop went up", asker.calls)
	}
	if !strings.Contains(buf.String(), "jev_progress: stopped: E-STOP active") {
		t.Errorf("log = %q, want the pass to say it stopped for the E-stop", buf.String())
	}
}
