package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// nowT is the clock every test runs on. The window is the fifteen minutes
// before it.
var nowT = time.Date(2026, 10, 21, 0, 30, 0, 0, time.UTC)

// fakeAsker records the state it was asked with and returns ans or err.
type fakeAsker struct {
	state any
	qs    map[string]jev.Question
	calls int
	ans   *jev.Answers
	err   error
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	f.calls++
	f.state = state
	f.qs = qs
	if f.err != nil {
		return nil, f.err
	}
	return f.ans, nil
}

// fixtureAnswers is one Ask result with every question of the battery
// answered.
func fixtureAnswers() *jev.Answers {
	return &jev.Answers{
		Model: jev.Model,
		Noul: map[string]float64{
			QMakingProgress:       0.9,
			QLooping:              0.1,
			QBlockedOnEnvironment: 0.2,
			QDriftedFromBead:      0.05,
		},
		Score: map[string]jev.ScoreAnswer{
			QRemaining: {Score: 2, Confidence: 0.8},
		},
	}
}

// line is one transcript line of the given type, stamped at ts, with blocks as
// its content.
func line(typ, ts string, blocks ...string) string {
	return fmt.Sprintf(`{"type":%q,"timestamp":%q,"message":{"role":%q,"content":[%s]}}`,
		typ, ts, typ, strings.Join(blocks, ","))
}

func textBlock(text string) string {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return string(b)
}

func toolUseBlock(name, command string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "tool_use", "id": "t", "name": name, "input": map[string]string{"command": command},
	})
	return string(b)
}

func toolResultBlock(content string) string {
	b, _ := json.Marshal(map[string]any{"type": "tool_result", "tool_use_id": "t", "content": content})
	return string(b)
}

// writeTranscript writes lines as a transcript under the town's Claude config
// dir, and returns its path.
func writeTranscript(t *testing.T, town string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(town, ".claude-town", "projects", "polecat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newPass is a pass over one town with the asker, the queue and a bead
// reader that returns one bead for every id.
func newPass(t *testing.T, town string, asker Asker, q *Queue) *Pass {
	t.Helper()
	return &Pass{
		Asker:    asker,
		Queue:    q,
		TownRoot: town,
		LogPath:  LogPath(town),
		Allows:   func() (bool, error) { return true, nil },
		Now:      func() time.Time { return nowT },
		Issue: func(rig, id string) (*beads.Issue, error) {
			return &beads.Issue{ID: id, Title: "Polecat progress battery"}, nil
		},
	}
}

func nudgeFor(transcript string) Nudge {
	return Nudge{
		Rig:        "gastown",
		Polecat:    "capable",
		Bead:       "gt-hurgx",
		Session:    "gt-gastown-capable",
		At:         nowT.Add(-time.Minute),
		Transcript: transcript,
	}
}

func readLog(t *testing.T, path string) []Line {
	t.Helper()
	var lines []Line
	if err := battery.ReadRecent(path, 100, &lines); err != nil {
		t.Fatal(err)
	}
	return lines
}

// A nudge queued by the guard gets one asked line with the five answers.
func TestJevProgressNudgeLogsOneLineWithFiveAnswers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTranscript(t, town,
		line("assistant", "2026-10-21T00:20:00Z", textBlock("Reading the guard.")),
	)
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}

	sum, err := newPass(t, town, asker, q).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Unanswered != 0 {
		t.Fatalf("summary = %+v, want one asked nudge", sum)
	}
	if asker.calls != 1 {
		t.Fatalf("Ask calls = %d, want 1", asker.calls)
	}

	lines := readLog(t, LogPath(town))
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	got := lines[0]
	if got.Bead != "gt-hurgx" || got.Polecat != "capable" || got.Session != "gt-gastown-capable" {
		t.Errorf("line names the wrong seat: %+v", got)
	}
	if got.Model != jev.Model || got.QuestionVersion != Version {
		t.Errorf("model/version = %q/%d", got.Model, got.QuestionVersion)
	}
	if len(got.Answers) != 5 {
		t.Fatalf("answers = %d, want 5: %v", len(got.Answers), got.Answers)
	}
	if a := got.Answers[QMakingProgress]; a.Noul == nil || *a.Noul != 0.9 {
		t.Errorf("making_progress = %+v", a)
	}
	if a := got.Answers[QRemaining]; a.Score == nil || *a.Score != 2 {
		t.Errorf("remaining = %+v", a)
	}
	if len(asker.qs) != 5 {
		t.Errorf("questions asked = %d, want 5", len(asker.qs))
	}
}

// The state reads only the last fifteen minutes, and trims each tool call and
// result to fieldRunes. Assistant text is not trimmed.
func TestJevProgressStateTrimsResultsAndStopsAtWindow(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	long := strings.Repeat("x", 1000)
	path := writeTranscript(t, town,
		line("assistant", "2026-10-21T00:05:00Z", textBlock("before the window")),
		line("assistant", "2026-10-21T00:20:00Z", toolUseBlock("Bash", "go test ./...")),
		line("user", "2026-10-21T00:21:00Z", toolResultBlock(long)),
		line("assistant", "2026-10-21T00:25:00Z", textBlock("inside the window")),
	)
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}

	if _, err := newPass(t, town, asker, q).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, ok := asker.state.(State)
	if !ok {
		t.Fatalf("state type = %T", asker.state)
	}
	if st.BeadTitle != "Polecat progress battery" || st.WindowMinutes != 15 {
		t.Errorf("header = %q, %d min", st.BeadTitle, st.WindowMinutes)
	}
	if len(st.Events) != 3 {
		t.Fatalf("events = %d, want 3 (the 00:05 text is outside the window)", len(st.Events))
	}
	for _, ev := range st.Events {
		if strings.Contains(ev.Text, "before the window") {
			t.Errorf("an event before the window reached the state: %+v", ev)
		}
	}
	result := st.Events[1]
	if result.Kind != "tool_result" {
		t.Fatalf("event 1 kind = %q", result.Kind)
	}
	if n := len([]rune(result.Text)); n > fieldRunes+1 {
		t.Errorf("tool result is %d runes, want at most %d", n, fieldRunes+1)
	}
	if st.Events[2].Text != "inside the window" {
		t.Errorf("assistant text = %q, want it whole", st.Events[2].Text)
	}
}

// A window larger than the token cap keeps its newest events and marks the
// count it dropped, and the state it sends stays within the cap.
func TestJevProgressStateCappedAtTokenBudget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var lines []string
	for i := range 60 {
		ts := nowT.Add(-10*time.Minute + time.Duration(i)*time.Second).Format(time.RFC3339)
		lines = append(lines, line("assistant", ts, textBlock(fmt.Sprintf("step %02d %s", i, strings.Repeat("y", 3000)))))
	}
	path := writeTranscript(t, town, lines...)
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}

	if _, err := newPass(t, town, asker, q).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := asker.state.(State)
	if st.EventsDropped == 0 {
		t.Fatalf("no events dropped from a %d-event window over the cap", len(lines))
	}
	if got := len(st.Events) + st.EventsDropped; got != len(lines) {
		t.Errorf("kept %d + dropped %d != %d events", len(st.Events), st.EventsDropped, len(lines))
	}
	last := st.Events[len(st.Events)-1]
	if !strings.HasPrefix(last.Text, "step 59 ") {
		t.Errorf("newest event was not kept: %q", last.Text[:20])
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxStateTokens*bytesPerToken {
		t.Errorf("state is %d bytes, over the %d-token cap", len(b), maxStateTokens)
	}
}

// A nudge with no transcript, an unreadable one and one outside the town each
// get a line with null answers and a reason, and the pass goes on to the next.
func TestJevProgressUnreadableNudgesLogReasonAndContinue(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	good := writeTranscript(t, town,
		line("assistant", "2026-10-21T00:25:00Z", textBlock("working")),
	)
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	q := &Queue{}
	q.Push(nudgeFor(""))
	q.Push(nudgeFor(filepath.Join(town, "missing.jsonl")))
	q.Push(nudgeFor(outside))
	q.Push(nudgeFor(good))
	asker := &fakeAsker{ans: fixtureAnswers()}

	sum, err := newPass(t, town, asker, q).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Unanswered != 3 {
		t.Fatalf("summary = %+v, want 1 asked and 3 unanswered", sum)
	}
	if asker.calls != 1 {
		t.Errorf("Ask calls = %d, want 1: an unreadable nudge must not reach Jev", asker.calls)
	}
	lines := readLog(t, LogPath(town))
	if len(lines) != 4 {
		t.Fatalf("log lines = %d, want 4", len(lines))
	}
	wantReasons := []string{ReasonNoTranscript, ReasonTranscriptUnreadable, ReasonOutsideTown, ""}
	for i, want := range wantReasons {
		if lines[i].Reason != want {
			t.Errorf("line %d reason = %q, want %q", i, lines[i].Reason, want)
		}
		if (want == "") == (lines[i].Answers == nil) {
			t.Errorf("line %d answers = %v with reason %q", i, lines[i].Answers, want)
		}
	}
}

// A symlink inside the town that points outside it is refused: the containment
// check runs on the resolved path, so the target is never opened.
func TestJevProgressSymlinkOutOfTownIsRefused(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	target := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(target, []byte(line("assistant", "2026-10-21T00:25:00Z", textBlock("escaped"))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(town, ".claude-town", "projects", "polecat", "linked.jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	q := &Queue{}
	q.Push(nudgeFor(link))
	asker := &fakeAsker{ans: fixtureAnswers()}

	if _, err := newPass(t, town, asker, q).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if asker.calls != 0 {
		t.Errorf("Ask calls = %d, want 0: a transcript outside the town must not be read", asker.calls)
	}
	lines := readLog(t, LogPath(town))
	if len(lines) != 1 || lines[0].Reason != ReasonOutsideTown {
		t.Fatalf("log = %+v, want one line with reason %s", lines, ReasonOutsideTown)
	}
}

// A town root that is itself a symlink still matches its transcripts: both
// sides are resolved before the check.
func TestJevProgressTownUnderASymlinkStillMatches(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	townLink := filepath.Join(t.TempDir(), "gt")
	if err := os.Symlink(town, townLink); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("working")))
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}

	if _, err := newPass(t, townLink, asker, q).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if asker.calls != 1 {
		t.Errorf("Ask calls = %d, want 1: a transcript under a symlinked town root is in the town", asker.calls)
	}
}

// A progress log that cannot be appended to ends the pass. The nudge whose line
// failed and every nudge after it count as dropped, and Run reports the error.
func TestJevProgressLogFailureStopsThePassAndCountsDrops(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	blocker := filepath.Join(town, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("working")))
	q := &Queue{}
	q.Push(nudgeFor(path))
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}
	p := newPass(t, town, asker, q)
	p.LogPath = filepath.Join(blocker, "progress.jsonl")

	sum, err := p.Run(context.Background())
	if err == nil {
		t.Fatalf("Run returned no error for an unwritable log; summary = %+v", sum)
	}
	if sum.Dropped != 2 || sum.Asked != 0 || sum.Unanswered != 0 {
		t.Errorf("summary = %+v, want 2 dropped and nothing counted", sum)
	}
}

// bulkLines returns n transcript lines of about a kilobyte each, stamped at ts.
// A few thousand of them are enough to push a transcript past the tail the
// pass reads.
func bulkLines(ts string, n int) []string {
	long := strings.Repeat("y", 1000)
	out := make([]string, n)
	for i := range out {
		out[i] = line("assistant", ts, textBlock(long))
	}
	return out
}

// A window whose start lies before the tail the pass reads is marked clipped:
// the events from its start are missing, and the state says so.
func TestJevProgressWindowClippedWhenTheTailMissesItsStart(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTranscript(t, town, bulkLines("2026-10-21T00:25:00Z", tailBytes/1000+1000)...)
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}

	if _, err := newPass(t, town, asker, q).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := asker.state.(State)
	if !st.WindowClipped {
		t.Error("window_clipped = false, want true: the window starts before the tail")
	}
}

// A file longer than the tail whose tail already starts before the window is
// not clipped: every event in the window is in the tail.
func TestJevProgressWindowNotClippedWhenTheTailStartsBeforeIt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	old := bulkLines("2026-10-21T00:05:00Z", tailBytes/1000+1000)
	recent := line("assistant", "2026-10-21T00:25:00Z", textBlock("recent"))
	path := writeTranscript(t, town, append(old, recent)...)
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}

	if _, err := newPass(t, town, asker, q).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := asker.state.(State)
	if st.WindowClipped {
		t.Error("window_clipped = true, want false: the tail covers the whole window")
	}
	if len(st.Events) != 1 || st.Events[0].Text != "recent" {
		t.Errorf("events = %+v, want the one recent event", st.Events)
	}
}

// A bead that cannot be read logs its nudge with null answers and does not
// reach Jev.
func TestJevProgressBeadUnreadableLogsReason(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("x")))
	q := &Queue{}
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}
	pass := newPass(t, town, asker, q)
	pass.Issue = func(string, string) (*beads.Issue, error) { return nil, errors.New("dolt down") }

	sum, err := pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unanswered != 1 || asker.calls != 0 {
		t.Fatalf("summary = %+v, calls = %d", sum, asker.calls)
	}
	if lines := readLog(t, LogPath(town)); len(lines) != 1 || lines[0].Reason != ReasonBeadUnreadable {
		t.Errorf("log = %+v", lines)
	}
}

// A response Jev could not use logs its nudge with a reason, and a failed call
// does not stop the pass.
func TestJevProgressBadResponseLogsReasonAndContinues(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("x")))
	q := &Queue{}
	q.Push(nudgeFor(path))
	q.Push(nudgeFor(path))
	asker := &fakeAsker{err: jev.ErrBadResponse}

	sum, err := newPass(t, town, asker, q).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unanswered != 2 || asker.calls != 2 {
		t.Fatalf("summary = %+v, calls = %d", sum, asker.calls)
	}
	for _, l := range readLog(t, LogPath(town)) {
		if l.Reason != ReasonBadResponse {
			t.Errorf("reason = %q, want %q", l.Reason, ReasonBadResponse)
		}
	}
}

// Once the day's spend is at the cap the pass asks nothing and drops what it
// did not reach, so the queue is empty for the next nudge.
func TestJevProgressCapReachedDropsTheRest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("x")))
	q := &Queue{}
	q.Push(nudgeFor(path))
	q.Push(nudgeFor(path))
	asker := &fakeAsker{ans: fixtureAnswers()}
	pass := newPass(t, town, asker, q)
	pass.Allows = func() (bool, error) { return false, nil }

	sum, err := pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Dropped != 2 || asker.calls != 0 {
		t.Fatalf("summary = %+v, calls = %d", sum, asker.calls)
	}
	if q.Len() != 0 {
		t.Errorf("queue holds %d nudges after the pass", q.Len())
	}
	if _, err := os.Stat(LogPath(town)); !os.IsNotExist(err) {
		t.Errorf("a capped pass wrote a log: %v", err)
	}
}

// The queue is bounded: a nudge past MaxQueued is refused, not held.
func TestJevProgressQueueIsBounded(t *testing.T) {
	t.Parallel()
	var q Queue
	for i := range MaxQueued {
		if !q.Push(Nudge{Bead: fmt.Sprint(i)}) {
			t.Fatalf("push %d refused under the bound", i)
		}
	}
	if q.Push(Nudge{Bead: "over"}) {
		t.Fatal("push past MaxQueued was accepted")
	}
	got := q.Drain()
	if len(got) != MaxQueued || got[0].Bead != "0" {
		t.Errorf("drained %d nudges, first %q", len(got), got[0].Bead)
	}
	if q.Len() != 0 {
		t.Errorf("queue holds %d after Drain", q.Len())
	}
}

// The battery's questions are the five the progress log names.
func TestJevProgressQuestionSet(t *testing.T) {
	t.Parallel()
	qs := Questions()
	want := map[string]string{
		QMakingProgress:       "noul",
		QLooping:              "noul",
		QBlockedOnEnvironment: "noul",
		QDriftedFromBead:      "noul",
		QRemaining:            "score",
	}
	if len(qs) != len(want) {
		t.Fatalf("questions = %d, want %d", len(qs), len(want))
	}
	for id, typ := range want {
		q, ok := qs[id]
		if !ok || q.Type != typ {
			t.Errorf("question %s = %+v, want type %s", id, q, typ)
		}
	}
	if got := qs[QRemaining].Levels; len(got) != 4 || got[0] != "done" {
		t.Errorf("remaining levels = %v", got)
	}
}
