package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

func TestJevSubmissionPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_submission") {
		t.Error("jev_submission must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_submission") {
		t.Error("jev_submission must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevSubmission: &JevSubmissionConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_submission") {
		t.Error("an explicit enabled=false must keep jev_submission off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevSubmission: &JevSubmissionConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_submission") {
		t.Error("an explicit enabled=true must turn jev_submission on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevSubmissionTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevSubmission()

	if d.jevSubmissionRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_submission"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, without
// reaching Jev, git or the rig's beads.
func TestJevSubmissionRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevSubmission: &JevSubmissionConfig{Enabled: true}},
		},
	}

	d.runJevSubmission()

	if !strings.Contains(logs.String(), "jev_submission: error:") ||
		!strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevSubmissionConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_submission":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevSubmission
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_submission = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// stubSubmissions lists the submissions a pass reads: each bead it holds, as a
// waiting entry at its READY TO LAND block, with no record subjects.
type stubSubmissions struct{ issues []*beads.Issue }

func (s stubSubmissions) Since(time.Time) ([]submission.Entry, error) {
	var out []submission.Entry
	for _, issue := range s.issues {
		w, ok := land.ParseReadyNote(issue.Notes)
		if !ok {
			continue
		}
		w.BeadID = issue.ID
		out = append(out, submission.Entry{Issue: issue, Work: w, Seen: w.Submitted})
	}
	return out, nil
}

// stubSubmissionGit reads a placeholder commit and diff: the daemon test is
// about when a pass runs and what it logs, not about the state it builds.
type stubSubmissionGit struct{ syncErr error }

func (g stubSubmissionGit) Sync(context.Context) error { return g.syncErr }

func (stubSubmissionGit) CommitSubjects(context.Context, string, string, int) ([]string, error) {
	return []string{"fix: a thing"}, nil
}

func (stubSubmissionGit) Numstat(context.Context, string, string) (string, error) {
	return "1\t0\tinternal/x.go\n", nil
}

// countingSubmissionAsker answers every Ask and counts them.
type countingSubmissionAsker struct{ n int }

func (c *countingSubmissionAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	c.n++
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul: map[string]float64{
			submission.QTestsCoverChange: 0.5, submission.QBeyondGoal: 0.5,
			submission.QCommitsMatchChange: 0.5, submission.QTouchesTownMachinery: 0.5,
		},
		Score: map[string]jev.ScoreAnswer{submission.QRisk: {Score: 1, Confidence: 0.5}},
	}, nil
}

// jevSubmissionRig is an enabled daemon over a temp town whose passes open
// through a fake, so no pass reaches Jev, git or the rig's beads.
type jevSubmissionRig struct {
	d      *Daemon
	clock  *clockwork.FakeClock
	town   string
	logs   *bytes.Buffer
	opened int
	got    submission.Settings
	pass   *submission.Pass
	asker  *countingSubmissionAsker
}

func newJevSubmissionRig(t *testing.T) *jevSubmissionRig {
	t.Helper()
	town := t.TempDir()
	r := &jevSubmissionRig{
		town: town, logs: &bytes.Buffer{}, asker: &countingSubmissionAsker{},
		clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevSubmission: &JevSubmissionConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	dayCap := battery.Cap{Ledger: jev.UsageLedgerPath(town), MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}
	r.pass = &submission.Pass{
		Asker:       r.asker,
		Submissions: stubSubmissions{},
		Git:         stubSubmissionGit{},
		LogPath:     submission.LogPath(town),
		CursorPath:  submission.CursorPath(town),
		Allows:      func() (bool, error) { return dayCap.Allows(r.clock.Now) },
		Now:         func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
	r.d.jevSubmissionOpen = func(_ string, s submission.Settings, _ io.Writer) (*submission.Pass, error) {
		r.opened++
		r.got = s
		return r.pass, nil
	}
	return r
}

// setSpendToday overwrites the usage ledger with one record of today's spend:
// inputTokens input tokens, which the rig prices at $3 per billion.
func (r *jevSubmissionRig) setSpendToday(t *testing.T, inputTokens int) {
	t.Helper()
	path := jev.UsageLedgerPath(r.town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"ts":%q,"caller":"jev-dedupe","input_tokens":%d,"output_tokens":0}`,
		r.clock.Now().Add(-time.Hour).Format(time.RFC3339), inputTokens)
	if err := os.WriteFile(path, []byte(rec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedCursor starts the pass's cursor so its next run reads submissions.
func (r *jevSubmissionRig) seedCursor(t *testing.T, at time.Time) {
	t.Helper()
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{Time: at}); err != nil {
		t.Fatal(err)
	}
}

func waitJevSubmissionIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevSubmissionRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_submission pass did not finish")
		}
		runtime.Gosched()
	}
}

// submitted is a work bead submitted for landing at.
func submitted(bead string, at time.Time) *beads.Issue {
	return &beads.Issue{
		ID: bead, Title: "bead " + bead, Status: "open",
		Notes:       land.FormatReadyNote(land.Work{Branch: "polecat/op/" + bead, Head: "h-" + bead, Target: "main", Worker: "op", Submitted: at}),
		Description: "## Goal\n\nDo it.\n",
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings, records the run, and then is not
// due again until the 15-minute interval passes.
func TestJevSubmissionTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)

	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (submission.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if _, err := os.Stat(submission.CursorPath(r.town)); err != nil {
		t.Errorf("the first pass left no cursor: %v", err)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_submission"); !found {
		t.Error("no last-run record after the pass")
	}

	if jevSubmissionInterval != 15*time.Minute {
		t.Fatalf("interval = %s, want 15m", jevSubmissionInterval)
	}
	r.clock.Advance(jevSubmissionInterval - 2*time.Minute)
	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a trigger inside the interval opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(2 * time.Minute)
	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
}

// A last-run save that fails still suppresses the next heartbeat's rerun for
// the interval. With no record on disk every check read the patrol as due, so a
// town whose daemon directory cannot be written opened a pass per heartbeat —
// each one reaching git and Jev for the submissions its cursor had not paid for
// yet (gt-4nqej, gt-th2am).
func TestJevSubmissionFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	// The last-run record cannot be written: its directory is a regular file.
	if err := os.WriteFile(filepath.Join(r.town, "daemon"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)
	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)

	if r.opened != 1 {
		t.Errorf("pass opened %d times inside one interval after a failed save, want 1", r.opened)
	}
	if !strings.Contains(r.logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", r.logs)
	}

	r.clock.Advance(jevSubmissionInterval)
	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
}

// A trigger that arrives while a pass is in flight is turned away.
func TestJevSubmissionTriggerSkipsWhileAPassIsRunning(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	r.d.jevSubmissionRunning.Store(true)

	r.d.triggerJevSubmission()

	if r.opened != 0 {
		t.Errorf("a trigger opened a pass while one was in flight")
	}
	if !r.d.jevSubmissionRunning.Load() {
		t.Error("the trigger released a guard it did not take")
	}
}

// An operator-disabled patrol stays off even with the config on.
func TestJevSubmissionTriggerHonorsDisabledPatrols(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	r.d.disabledPatrols = map[string]bool{"jev_submission": true}

	r.d.triggerJevSubmission()

	if r.opened != 0 {
		t.Error("a disabled patrol opened a pass")
	}
}

// A pass that reaches a submission logs one line for it and says so.
func TestJevSubmissionRunReportsTheSubmissionsAsked(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Submissions = stubSubmissions{issues: []*beads.Issue{
		submitted("gt-new", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
	}}

	r.d.runJevSubmission()

	if !strings.Contains(r.logs.String(), "jev_submission: asked 1 submissions") {
		t.Errorf("log = %q, want the asked count", r.logs)
	}
	lines, err := submission.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Bead != "gt-new" {
		t.Errorf("log lines = %+v, %v, want one line for gt-new", lines, err)
	}
}

// A submission the log already holds a line for at the current question
// version is passed over rather than asked twice, and the pass's line says how
// many it passed over.
func TestJevSubmissionRunReportsRepeatedSubmissions(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	cursor := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	r.seedCursor(t, cursor)
	r.pass.Submissions = stubSubmissions{issues: []*beads.Issue{
		submitted("gt-new", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
	}}

	r.d.runJevSubmission()
	// The same submission reaches a second pass, as it does when a later
	// first-sight record of it falls past the cursor.
	r.seedCursor(t, cursor)
	r.d.runJevSubmission()

	if r.asker.n != 1 {
		t.Errorf("Jev was asked %d times, want once", r.asker.n)
	}
	if !strings.Contains(r.logs.String(), "repeated 1") {
		t.Errorf("log = %q, want the repeated count", r.logs)
	}
	lines, err := submission.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 {
		t.Errorf("log lines = %+v, %v, want one", lines, err)
	}
}

// When the cap check says the day's spend is used up, the pass asks nothing,
// logs nothing and says the cap was reached; the submissions wait for a later
// pass.
func TestJevSubmissionRunStopsForTheDayWhenTheCapSaysSo(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Submissions = stubSubmissions{issues: []*beads.Issue{
		submitted("gt-new", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
	}}
	r.setSpendToday(t, 1_000_000_000) // $3 spent against the $2.5 cap

	r.d.runJevSubmission()

	if !strings.Contains(r.logs.String(), "asked 0 submissions") || !strings.Contains(r.logs.String(), "daily cap reached: true") {
		t.Errorf("log = %q, want the cap reached", r.logs)
	}
	if r.asker.n != 0 {
		t.Errorf("a capped pass made %d Jev calls", r.asker.n)
	}
	if _, err := os.Stat(r.pass.LogPath); !os.IsNotExist(err) {
		t.Errorf("a capped pass wrote a log: %v", err)
	}

	r.setSpendToday(t, 0)
	r.d.runJevSubmission()
	if r.asker.n != 1 {
		t.Errorf("the pass after the cap reopened made %d Jev calls, want 1", r.asker.n)
	}
}

// A clone that cannot be fetched skips the pass with a warning, not an error,
// and leaves the cursor where it was.
func TestJevSubmissionRunWarnsWhenTheFetchFails(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Git = stubSubmissionGit{syncErr: errors.New("could not resolve host")}
	r.pass.Submissions = stubSubmissions{issues: []*beads.Issue{
		submitted("gt-new", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
	}}

	r.d.triggerJevSubmission()
	waitJevSubmissionIdle(t, r.d)

	if !strings.Contains(r.logs.String(), "jev_submission: skipped:") || strings.Contains(r.logs.String(), "error:") {
		t.Errorf("log = %q, want a skipped line and no error", r.logs)
	}
	if r.asker.n != 0 {
		t.Errorf("a pass whose fetch failed made %d Jev calls", r.asker.n)
	}
	cur, ok, err := battery.ReadCursor(r.pass.CursorPath)
	if err != nil || !ok || !cur.Time.Equal(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("cursor = %+v ok=%v err=%v, want it where it was", cur, ok, err)
	}
}

// TestJevSubmissionPassStopsBetweenCallsWhenEstopped covers gt-tevxz: the E-stop
// kept a pass from starting, but one already in flight went on paying for every
// submission it reached. The call in flight finishes and is recorded; the
// submissions behind it wait for a pass run after the E-stop lifts.
func TestJevSubmissionPassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	r := newJevSubmissionRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Submissions = stubSubmissions{issues: []*beads.Issue{
		submitted("gt-a", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
		submitted("gt-b", time.Date(2026, 10, 20, 11, 1, 0, 0, time.UTC)),
		submitted("gt-c", time.Date(2026, 10, 20, 11, 2, 0, 0, time.UTC)),
	}}
	asker := &estoppingAsker{t: t, town: r.town}
	r.pass.Asker = asker

	r.d.runJevSubmission()

	if asker.calls != 1 {
		t.Errorf("Jev was asked about %d submissions, want only the one in flight when the E-stop went up", asker.calls)
	}
	lines, err := submission.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Bead != "gt-a" {
		t.Errorf("log lines = %+v, %v, want the one gt-a line recorded", lines, err)
	}
	if !strings.Contains(r.logs.String(), "jev_submission: stopped:") {
		t.Errorf("log = %q, want the pass to say it stopped for the E-stop", r.logs)
	}
}
